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

// addTenant makes a second tenant for a tenant-scope run; its rows go with it.
func addTenant(t *testing.T, db *ent.Client) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("PKB " + tid.String()).SetSlug("pkb-" + tid.String()).SetBusinessType("generic").SaveX(ctx)
	t.Cleanup(func() {
		_, _ = db.KnowledgeBaseEntry.Delete().Where(knowledgebaseentry.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	return tid
}

func runSet(t *testing.T, raw *sql.DB, set SeedSet, scope string, tid uuid.UUID, entries []PlatformEntry) PlatformSyncResult {
	t.Helper()
	res, err := SyncSeedSet(context.Background(), raw, set, scope, tid, entries)
	if err != nil {
		t.Fatalf("sync %s: %v", set.Name, err)
	}
	return res
}

func setEntry(category, title, content string) PlatformEntry {
	return PlatformEntry{Category: category, Title: title, Content: content, Tags: []string{"kenya"},
		Metadata: map[string]any{"source": "kenya-tax-reference.md"}}
}

func staleOf(t *testing.T, db *ent.Client, id uuid.UUID) bool {
	t.Helper()
	stale, _ := db.KnowledgeBaseEntry.GetX(context.Background(), id).Metadata[metaStale].(bool)
	return stale
}

// Seed sets keep to their own rows: each creates, rewrites and retires its own, in the scope and
// tenant it runs for, and never another set's, another scope's or another tenant's.
func TestSeedSetsKeepToThemselves(t *testing.T) {
	raw, db, tid := openPlatformDB(t)
	ctx := context.Background()
	sfx := tid.String()[:8]
	other := addTenant(t, db)

	tax := SeedSet{Name: "tax", SourceKind: "test_tax_" + sfx}
	faq := SeedSet{Name: "faq", SourceKind: "test_faq_" + sfx}
	vat := setEntry("compliance", "VAT "+sfx, "VAT 16% standard.")
	paye := setEntry("compliance", "PAYE "+sfx, "PAYE by the 9th.")
	// The same category and title in a second set is that set's own entry.
	faqVat := setEntry("compliance", "VAT "+sfx, "FAQ words about VAT.")
	cv := platformEntry("pricing_plans", "Plans "+sfx, "Starter 2,500 a month.")

	if got := runSet(t, raw, tax, ScopePlatform, tid, []PlatformEntry{vat, paye}); got.Created != 2 {
		t.Fatalf("tax: %+v", got)
	}
	if got := runSet(t, raw, faq, ScopePlatform, tid, []PlatformEntry{faqVat}); got.Created != 1 || len(got.Skipped) != 0 {
		t.Fatalf("faq: %+v", got)
	}
	runPlatformSync(t, raw, tid, []PlatformEntry{cv})
	// The same set in tenant scope for another tenant: that tenant's own copy.
	if got := runSet(t, raw, tax, ScopeTenant, other, []PlatformEntry{vat, paye}); got.Created != 2 {
		t.Fatalf("tenant run: %+v", got)
	}

	idVat := SeedEntryID(tax.SourceKind, ScopePlatform, tid, vat.Category, vat.Title)
	idPaye := SeedEntryID(tax.SourceKind, ScopePlatform, tid, paye.Category, paye.Title)
	idFaqVat := SeedEntryID(faq.SourceKind, ScopePlatform, tid, faqVat.Category, faqVat.Title)
	idCv := PlatformEntryID(cv.Category, cv.Title)
	idTenantPaye := SeedEntryID(tax.SourceKind, ScopeTenant, other, paye.Category, paye.Title)

	if r := db.KnowledgeBaseEntry.GetX(ctx, idVat); r.Metadata[metaSourceKind] != tax.SourceKind || r.Scope != ScopePlatform || r.TenantID != tid {
		t.Fatalf("tax row: %+v", r)
	}
	if r := db.KnowledgeBaseEntry.GetX(ctx, idTenantPaye); r.Scope != ScopeTenant || r.TenantID != other {
		t.Fatalf("tenant copy: %+v", r)
	}
	for _, id := range []uuid.UUID{idVat, idPaye, idFaqVat, idCv} {
		addEmbedding(t, raw, id, tid)
	}
	addEmbedding(t, raw, idTenantPaye, other)

	// Removing PAYE from the tax set's platform run retires only that row.
	if got := runSet(t, raw, tax, ScopePlatform, tid, []PlatformEntry{vat}); got.Retired != 1 || got.Unchanged != 1 {
		t.Fatalf("tax retire: %+v", got)
	}
	if !staleOf(t, db, idPaye) {
		t.Fatal("the removed entry is retired")
	}
	for _, id := range []uuid.UUID{idVat, idFaqVat, idCv, idTenantPaye} {
		if staleOf(t, db, id) || embeddingCount(t, raw, id) != 1 {
			t.Fatalf("row %s of another set, scope or tenant was touched", id)
		}
	}

	// Rewriting the FAQ set leaves the tax set alone, and the Codevertex sync retires nothing of theirs.
	faqVat.Content = "FAQ words about VAT, reworded."
	if got := runSet(t, raw, faq, ScopePlatform, tid, []PlatformEntry{faqVat}); got.Updated != 1 {
		t.Fatalf("faq rewrite: %+v", got)
	}
	if got := runPlatformSync(t, raw, tid, []PlatformEntry{cv}); got.Unchanged != 1 || got.Retired != 0 {
		t.Fatalf("codevertex: %+v", got)
	}
	if db.KnowledgeBaseEntry.GetX(ctx, idVat).Content != vat.Content || embeddingCount(t, raw, idVat) != 1 {
		t.Fatal("the tax set's VAT entry was touched by another set")
	}

	// The tenant-scope run retires in its own tenant only.
	if got := runSet(t, raw, tax, ScopeTenant, other, []PlatformEntry{vat}); got.Retired != 1 {
		t.Fatalf("tenant retire: %+v", got)
	}
	if !staleOf(t, db, idTenantPaye) || staleOf(t, db, idVat) {
		t.Fatal("tenant retirement crossed scopes")
	}
}

// Each set retires the rows its old create-only seed made, once and only its own: by title, or by
// product code for products (a product renamed since still matches). Rows synced from a
// business's system, hand-written rows and other sets' legacy rows stay.
func TestSeedSetLegacyRetiredPerSet(t *testing.T) {
	raw, db, tid := openPlatformDB(t)
	ctx := context.Background()
	sfx := tid.String()[:8]
	code := "9" + sfx[:5]
	cc := 0.102

	taxTitle, faqTitle := "Kenya tax "+sfx, "What is FLP "+sfx+"?"
	tax := SeedSet{Name: "tax", SourceKind: "test_tax_" + sfx,
		Legacy: []LegacySeedRow{{Category: "compliance", Title: taxTitle}}}
	products := SeedSet{Name: "products", SourceKind: "test_products_" + sfx,
		Legacy: []LegacySeedRow{{Category: "product", ProductCode: code}}}
	faq := SeedSet{Name: "faq", SourceKind: "test_faq_" + sfx,
		Legacy: []LegacySeedRow{{Category: "faq", Title: faqTitle}}}

	mk := func(scope, category, title string) *ent.KnowledgeBaseEntryCreate {
		return db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetScope(scope).SetCategory(category).SetTitle(title).SetContent("old words")
	}
	legacyTax := mk("platform", "compliance", taxTitle).SaveX(ctx)
	legacyProduct := mk("platform", "product", "Old name "+sfx).SetProductCode(code).SetProductCc(cc).SaveX(ctx)
	legacyFaq := mk("platform", "faq", faqTitle).SaveX(ctx)
	// The same title in tenant scope is not the platform run's.
	tenantTax := mk("tenant", "compliance", taxTitle).SaveX(ctx)
	// A row synced from a business's system carrying the same product code.
	synced := mk("platform", "product", "Aloe gel "+sfx).SetProductCode(code).
		SetMetadata(map[string]any{metaSource: SourceInventory, metaSourceID: "x"}).SaveX(ctx)
	// A hand-written platform row with a title the set does not list.
	hand := mk("platform", "compliance", "Our own tax note "+sfx).SaveX(ctx)

	taxEntry := setEntry("compliance", taxTitle, "Kenya tax words.")
	got := runSet(t, raw, tax, ScopePlatform, tid, []PlatformEntry{taxEntry})
	if got.Retired != 1 || got.Created != 1 || len(got.Skipped) != 0 {
		t.Fatalf("tax: %+v, want the legacy row retired and the entry created in its place", got)
	}
	if !staleOf(t, db, legacyTax.ID) {
		t.Fatal("the tax set's legacy row is retired")
	}
	for _, r := range []*ent.KnowledgeBaseEntry{legacyProduct, legacyFaq, tenantTax, synced, hand} {
		if staleOf(t, db, r.ID) {
			t.Fatalf("row %q is not the tax set's legacy row", r.Title)
		}
	}

	pc := code
	product := PlatformEntry{Category: "product", Title: "Forever Aloe Vera Gel " + sfx, Content: "Aloe words.", ProductCode: &pc, ProductCC: &cc}
	if got := runSet(t, raw, products, ScopePlatform, tid, []PlatformEntry{product}); got.Retired != 1 || got.Created != 1 {
		t.Fatalf("products: %+v", got)
	}
	if !staleOf(t, db, legacyProduct.ID) || staleOf(t, db, synced.ID) {
		t.Fatal("the renamed legacy product is retired by its code; the synced row stays")
	}
	r := db.KnowledgeBaseEntry.GetX(ctx, SeedEntryID(products.SourceKind, ScopePlatform, tid, product.Category, product.Title))
	if r.ProductCode == nil || *r.ProductCode != code || r.ProductCc == nil || *r.ProductCc != cc {
		t.Fatalf("product fields: %v %v", r.ProductCode, r.ProductCc)
	}

	// Once: a second run finds no legacy row left, and the FAQ legacy row still waits for its own set.
	if got := runSet(t, raw, tax, ScopePlatform, tid, []PlatformEntry{taxEntry}); got.Retired != 0 || got.Unchanged != 1 {
		t.Fatalf("second tax run: %+v", got)
	}
	if staleOf(t, db, legacyFaq.ID) {
		t.Fatal("another set's legacy row was retired")
	}
	if got := runSet(t, raw, faq, ScopePlatform, tid, []PlatformEntry{setEntry("faq", faqTitle, "FLP words.")}); got.Retired != 1 || got.Created != 1 {
		t.Fatalf("faq: %+v", got)
	}
	if staleOf(t, db, tenantTax.ID) || staleOf(t, db, hand.ID) {
		t.Fatal("tenant and hand-written rows are never retired")
	}
}

// A platform-only set refuses a tenant run, and a hand-written tenant row blocks a tenant-scope
// seed entry with the same title in that tenant only.
func TestSeedSetScopeRules(t *testing.T) {
	raw, db, tid := openPlatformDB(t)
	ctx := context.Background()
	sfx := tid.String()[:8]
	other := addTenant(t, db)

	if _, err := SyncSeedSet(ctx, raw, CodevertexSeedSet(), ScopeTenant, tid, []PlatformEntry{platformEntry("c", "t "+sfx, "x")}); err == nil {
		t.Fatal("the Codevertex set is platform only")
	}
	set := SeedSet{Name: "tax", SourceKind: "test_tax_" + sfx}
	e := setEntry("compliance", "VAT "+sfx, "VAT 16%.")
	db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetScope("tenant").SetCategory(e.Category).SetTitle(e.Title).SetContent("our own").SaveX(ctx)

	if got := runSet(t, raw, set, ScopeTenant, tid, []PlatformEntry{e}); len(got.Skipped) != 1 {
		t.Fatalf("own tenant: %+v, want the hand-written row to win", got)
	}
	if got := runSet(t, raw, set, ScopeTenant, other, []PlatformEntry{e}); got.Created != 1 {
		t.Fatalf("other tenant: %+v", got)
	}
	if got := runSet(t, raw, set, ScopePlatform, tid, []PlatformEntry{e}); got.Created != 1 {
		t.Fatalf("platform: %+v, a tenant's row does not block the platform entry", got)
	}
}

// Every pod runs every set on start. Pods running all sets at once create each entry once,
// rewrite a changed entry once and retire a removed entry once.
func TestSeedSetsConcurrentRuns(t *testing.T) {
	raw, db, tid := openPlatformDB(t)
	ctx := context.Background()
	sfx := tid.String()[:8]

	type run struct {
		set     SeedSet
		entries []PlatformEntry
	}
	var runs []run
	for _, name := range []string{"tax", "products", "faq"} {
		var entries []PlatformEntry
		for _, n := range []string{"one", "two", "three"} {
			entries = append(entries, setEntry("c_"+name, n+" "+sfx, name+" "+n+"."))
		}
		runs = append(runs, run{SeedSet{Name: name, SourceKind: "test_" + name + "_" + sfx}, entries})
	}

	runAll := func() PlatformSyncResult {
		var (
			mu    sync.Mutex
			total PlatformSyncResult
			wg    sync.WaitGroup
		)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, r := range runs {
					res, err := SyncSeedSet(ctx, raw, r.set, ScopePlatform, tid, r.entries)
					if err != nil {
						t.Errorf("concurrent sync %s: %v", r.set.Name, err)
						return
					}
					mu.Lock()
					total.Created += res.Created
					total.Updated += res.Updated
					total.Retired += res.Retired
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return total
	}

	if got := runAll(); got.Created != 9 {
		t.Fatalf("created %d across pods, want 9", got.Created)
	}
	if n := db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.TenantID(tid)).CountX(ctx); n != 9 {
		t.Fatalf("%d rows, want 9", n)
	}
	runs[0].entries[1].Content = "tax two, reworded."
	runs[2].entries = runs[2].entries[:2]
	if got := runAll(); got.Updated != 1 || got.Retired != 1 {
		t.Fatalf("got %+v across pods, want one rewrite and one retirement", got)
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
