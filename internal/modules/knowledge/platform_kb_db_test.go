package knowledge

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// openPlatformDB opens the local test database (testDSN, sync_test.go) with a fresh tenant that
// stands in for the platform owner. Every row a test makes lives under that tenant and goes with it.
func openPlatformDB(t *testing.T) (*sql.DB, *ent.Client, uuid.UUID) {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	raw, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := raw.Ping(); err != nil {
		_ = raw.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	// The sync deletes search vectors, so the pgvector table must exist (the migrations make it).
	var embeddings sql.NullString
	if err := raw.QueryRow(`SELECT to_regclass('knowledge_base_embeddings')::text`).Scan(&embeddings); err != nil || !embeddings.Valid {
		_ = raw.Close()
		t.Skip("knowledge_base_embeddings missing locally; run the migrations")
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, raw)))
	ctx := context.Background()
	tid := uuid.New()
	client.Tenant.Create().SetID(tid).SetName("PKB " + tid.String()).SetSlug("pkb-" + tid.String()).SetBusinessType("generic").SaveX(ctx)
	t.Cleanup(func() {
		_, _ = client.KnowledgeBaseEntry.Delete().Where(knowledgebaseentry.TenantID(tid)).Exec(ctx)
		_ = client.Tenant.DeleteOneID(tid).Exec(ctx)
		_ = client.Close()
	})
	return raw, client, tid
}

func platformEntry(category, title, content string) PlatformEntry {
	return PlatformEntry{Scope: "platform", Category: category, Title: title, Content: content,
		Tags: []string{"codevertex"}, Metadata: map[string]any{"source": "codevertex-website/src/config/plans.ts", "checked": "2026-11-17"}}
}

func runPlatformSync(t *testing.T, raw *sql.DB, tid uuid.UUID, entries []PlatformEntry) PlatformSyncResult {
	t.Helper()
	res, err := SyncPlatformKB(context.Background(), raw, tid, entries)
	if err != nil {
		t.Fatalf("sync platform kb: %v", err)
	}
	return res
}

// addEmbedding stands in for the embed sweep: one search vector and embedded = true.
func addEmbedding(t *testing.T, raw *sql.DB, id, tid uuid.UUID) {
	t.Helper()
	vec := "[" + strings.TrimSuffix(strings.Repeat("0.1,", 768), ",") + "]"
	if _, err := raw.Exec(`INSERT INTO knowledge_base_embeddings (entry_id, tenant_id, chunk_text, chunk_index, category, title, embedding)
		VALUES ($1, $2, 'chunk', 0, 'c', 't', $3::vector)`, id, tid, vec); err != nil {
		t.Fatalf("add embedding: %v", err)
	}
	if _, err := raw.Exec(`UPDATE knowledge_base_entries SET embedded = true, chunk_count = 1 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func embeddingCount(t *testing.T, raw *sql.DB, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM knowledge_base_embeddings WHERE entry_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The upsert decision, step by step over one platform knowledge base: create, leave alone,
// rewrite and re-queue on change (words, metadata or a portal edit), retire on removal, revive.
func TestPlatformKBUpsertDecisions(t *testing.T) {
	raw, db, tid := openPlatformDB(t)
	ctx := context.Background()
	sfx := tid.String()[:8]

	a := platformEntry("platform_overview", "About "+sfx, "Codevertex Africa Limited is based in Kisumu.")
	b := platformEntry("pricing_plans", "Plans "+sfx, "Starter 4,999 a month.")
	bPrice := b
	bPrice.Content = "Starter 2,500 a month."
	bChecked := bPrice
	bChecked.Metadata = map[string]any{"source": "codevertex-website/src/config/plans.ts", "checked": "2026-12-01"}
	idA, idB := PlatformEntryID(a.Category, a.Title), PlatformEntryID(b.Category, b.Title)

	get := func(id uuid.UUID) *ent.KnowledgeBaseEntry { return db.KnowledgeBaseEntry.GetX(ctx, id) }

	steps := []struct {
		name    string
		setup   func(t *testing.T)
		entries []PlatformEntry
		want    PlatformSyncResult
		check   func(t *testing.T)
	}{
		{
			name:    "missing entries are created and queued for embedding",
			entries: []PlatformEntry{a, b},
			want:    PlatformSyncResult{Created: 2},
			check: func(t *testing.T) {
				r := get(idB)
				if r.Scope != "platform" || r.TenantID != tid || r.Embedded || r.Content != b.Content {
					t.Fatalf("created row: %+v", r)
				}
				if r.Metadata[metaSourceKind] != SourceKindPlatformSeed || r.Metadata[metaSeedSource] != "codevertex-website/src/config/plans.ts" || r.Metadata["checked"] != "2026-11-17" {
					t.Fatalf("created metadata: %v", r.Metadata)
				}
				if _, ok := r.Metadata[metaSource]; ok {
					t.Fatal("the file's source is stored as seed_source; source marks a synced entry")
				}
			},
		},
		{
			name: "an unchanged entry keeps its embeddings",
			setup: func(t *testing.T) {
				addEmbedding(t, raw, idA, tid)
				addEmbedding(t, raw, idB, tid)
			},
			entries: []PlatformEntry{a, b},
			want:    PlatformSyncResult{Unchanged: 2},
			check: func(t *testing.T) {
				if !get(idB).Embedded || embeddingCount(t, raw, idB) != 1 {
					t.Fatal("an unchanged entry is not re-embedded")
				}
			},
		},
		{
			name:    "changed words rewrite the entry and clear its embeddings",
			entries: []PlatformEntry{a, bPrice},
			want:    PlatformSyncResult{Updated: 1, Unchanged: 1},
			check: func(t *testing.T) {
				r := get(idB)
				if r.Content != bPrice.Content || r.Embedded || r.ChunkCount != 0 || embeddingCount(t, raw, idB) != 0 {
					t.Fatalf("rewritten row: content=%q embedded=%v chunks=%d", r.Content, r.Embedded, r.ChunkCount)
				}
				if embeddingCount(t, raw, idA) != 1 {
					t.Fatal("the unchanged neighbour keeps its embeddings")
				}
			},
		},
		{
			name:    "changed metadata rewrites the entry",
			setup:   func(t *testing.T) { addEmbedding(t, raw, idB, tid) },
			entries: []PlatformEntry{a, bChecked},
			want:    PlatformSyncResult{Updated: 1, Unchanged: 1},
			check: func(t *testing.T) {
				if r := get(idB); r.Metadata["checked"] != "2026-12-01" || r.Embedded || embeddingCount(t, raw, idB) != 0 {
					t.Fatalf("metadata rewrite: %v embedded=%v", r.Metadata, r.Embedded)
				}
			},
		},
		{
			name: "a portal edit of a seeded entry is put back to the file",
			setup: func(t *testing.T) {
				db.KnowledgeBaseEntry.UpdateOneID(idA).SetContent("edited in the portal").ExecX(ctx)
			},
			entries: []PlatformEntry{a, bChecked},
			want:    PlatformSyncResult{Updated: 1, Unchanged: 1},
			check: func(t *testing.T) {
				if get(idA).Content != a.Content {
					t.Fatal("the file wins over a portal edit")
				}
			},
		},
		{
			name:    "an entry removed from the file is retired, not deleted",
			setup:   func(t *testing.T) { addEmbedding(t, raw, idB, tid) },
			entries: []PlatformEntry{a},
			want:    PlatformSyncResult{Unchanged: 1, Retired: 1},
			check: func(t *testing.T) {
				r := get(idB)
				if r.Metadata[metaStale] != true || r.Metadata[metaStaleAt] == nil || !r.Embedded || r.ChunkCount != 0 || embeddingCount(t, raw, idB) != 0 {
					t.Fatalf("retired row: %v embedded=%v chunks=%d", r.Metadata, r.Embedded, r.ChunkCount)
				}
			},
		},
		{
			name:    "a retired entry is retired once",
			entries: []PlatformEntry{a},
			want:    PlatformSyncResult{Unchanged: 1},
		},
		{
			name:    "an entry put back in the file returns under its id",
			entries: []PlatformEntry{a, bChecked},
			want:    PlatformSyncResult{Updated: 1, Unchanged: 1},
			check: func(t *testing.T) {
				r := get(idB)
				if _, stale := r.Metadata[metaStale]; stale || r.Embedded {
					t.Fatalf("revived row: %v embedded=%v", r.Metadata, r.Embedded)
				}
			},
		},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if s.setup != nil {
				s.setup(t)
			}
			got := runPlatformSync(t, raw, tid, s.entries)
			if got.Created != s.want.Created || got.Updated != s.want.Updated || got.Unchanged != s.want.Unchanged ||
				got.Retired != s.want.Retired || len(got.Skipped) != len(s.want.Skipped) {
				t.Fatalf("got %+v, want %+v", got, s.want)
			}
			if s.check != nil {
				s.check(t)
			}
		})
	}
}

// Tenant rows and hand-written platform rows are never touched; the old hard-coded seed's rows
// are retired once.
func TestPlatformKBLeavesOtherRowsAlone(t *testing.T) {
	raw, db, tid := openPlatformDB(t)
	ctx := context.Background()
	sfx := tid.String()[:8]

	shared := platformEntry("erp", "ERP Suite "+sfx, "ERP covers HR, payroll and procurement.")
	clash := platformEntry("library", "Library "+sfx, "Library management for schools.")

	// A tenant's own entry with the same category and title as a file entry.
	tenantRow := db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetScope("tenant").SetCategory(shared.Category).
		SetTitle(shared.Title).SetContent("our own words").SaveX(ctx)
	// A tenant row that somehow carries the marker: still not the seed's to retire.
	markedTenantRow := db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetScope("tenant").SetCategory("custom_services").
		SetTitle("Marked " + sfx).SetContent("tenant words").SetMetadata(map[string]any{metaSourceKind: SourceKindPlatformSeed}).SaveX(ctx)
	// Hand-written platform rows: one unrelated, one with the same key as a file entry.
	handRow := db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetScope("platform").SetCategory("custom_services").
		SetTitle("Hand " + sfx).SetContent("written in the portal").SaveX(ctx)
	handClash := db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetScope("platform").SetCategory(clash.Category).
		SetTitle(clash.Title).SetContent("the owner's own library text").SaveX(ctx)
	// A row the old hard-coded seed created.
	legacy := db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetScope("platform").SetCategory("product").
		SetTitle("Power Suite " + emDash + " ERP").SetContent("Pricing: Growth or Enterprise plan.").SaveX(ctx)

	got := runPlatformSync(t, raw, tid, []PlatformEntry{shared, clash})
	if got.Created != 1 || got.Retired != 1 || len(got.Skipped) != 1 {
		t.Fatalf("got %+v, want 1 created, 1 retired (legacy), 1 skipped (hand-written clash)", got)
	}

	for _, r := range []*ent.KnowledgeBaseEntry{tenantRow, markedTenantRow, handRow, handClash} {
		now := db.KnowledgeBaseEntry.GetX(ctx, r.ID)
		if now.Content != r.Content || now.Scope != r.Scope || now.Embedded != r.Embedded || len(now.Metadata) != len(r.Metadata) {
			t.Fatalf("row %q was touched: %+v", r.Title, now)
		}
	}
	if db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.IDEQ(PlatformEntryID(clash.Category, clash.Title))).ExistX(ctx) {
		t.Fatal("no rival entry is added next to a hand-written one")
	}
	if !db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.IDEQ(PlatformEntryID(shared.Category, shared.Title))).ExistX(ctx) {
		t.Fatal("a tenant's entry does not block the platform one")
	}
	if r := db.KnowledgeBaseEntry.GetX(ctx, legacy.ID); r.Metadata[metaStale] != true {
		t.Fatalf("the old seed's row is retired: %v", r.Metadata)
	}
}

// Every API pod runs the seed on start. Runs at the same moment create each entry once and
// rewrite a changed entry once.
func TestPlatformKBConcurrentRuns(t *testing.T) {
	raw, db, tid := openPlatformDB(t)
	ctx := context.Background()
	sfx := tid.String()[:8]
	var entries []PlatformEntry
	for _, n := range []string{"one", "two", "three", "four", "five"} {
		entries = append(entries, platformEntry("onboarding_howto", n+" "+sfx, "Step "+n+"."))
	}

	runAll := func(entries []PlatformEntry) PlatformSyncResult {
		var (
			mu    sync.Mutex
			total PlatformSyncResult
			wg    sync.WaitGroup
		)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := SyncPlatformKB(ctx, raw, tid, entries)
				if err != nil {
					t.Errorf("concurrent sync: %v", err)
					return
				}
				mu.Lock()
				total.Created += res.Created
				total.Updated += res.Updated
				mu.Unlock()
			}()
		}
		wg.Wait()
		return total
	}

	if got := runAll(entries); got.Created != len(entries) {
		t.Fatalf("created %d across pods, want %d", got.Created, len(entries))
	}
	if n := db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.TenantID(tid)).CountX(ctx); n != len(entries) {
		t.Fatalf("%d rows, want %d", n, len(entries))
	}
	entries[2].Content = "Step three, reworded."
	if got := runAll(entries); got.Updated != 1 {
		t.Fatalf("rewrote %d times across pods, want once", got.Updated)
	}
}
