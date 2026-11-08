package knowledge

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const testDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openSyncDB(t *testing.T) (*ent.Client, uuid.UUID) {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	ctx := context.Background()
	tid := uuid.New()
	client.Tenant.Create().SetID(tid).SetName("KB " + tid.String()).SetSlug("kb-" + tid.String()).SetBusinessType("generic").SaveX(ctx)
	t.Cleanup(func() {
		_, _ = client.KnowledgeBaseEntry.Delete().Where(knowledgebaseentry.TenantID(tid)).Exec(ctx)
		_ = client.Tenant.DeleteOneID(tid).Exec(ctx)
		_ = client.Close()
	})
	return client, tid
}

func price(v float64) *float64 { return &v }

func apply(t *testing.T, s *Sync, it SourceItem, want Outcome) {
	t.Helper()
	got, err := s.Apply(context.Background(), it)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != want {
		t.Fatalf("apply %q: got %s, want %s", it.Name, got, want)
	}
}

// A synced entry is created once, untouched while nothing changed, and rewritten and re-embedded
// when the source changes. An event keeps the walk's real price.
func TestSyncCreateUpdateUnchanged(t *testing.T) {
	db, tid := openSyncDB(t)
	s := NewSync(db, nil)
	ctx := context.Background()
	it := SourceItem{TenantID: tid, Source: SourceInventory, SourceID: "i1", Kind: "GOODS", Name: "Shea Lotion", Description: "250ml", Price: price(850), Sellable: true}
	apply(t, s, it, Created)
	id := EntryID(tid, SourceInventory, "i1")
	row := db.KnowledgeBaseEntry.GetX(ctx, id)
	if row.Embedded || row.Category != "product" || *row.ProductPrice != 850 {
		t.Fatalf("created: %+v", row)
	}
	db.KnowledgeBaseEntry.UpdateOneID(id).SetEmbedded(true).ExecX(ctx)
	apply(t, s, it, Unchanged)
	if !db.KnowledgeBaseEntry.GetX(ctx, id).Embedded {
		t.Fatal("an unchanged item is not re-embedded")
	}

	// An event has no price and an unchanged description: same text, nothing to do.
	event := it
	event.Price, event.UpdateOnly = nil, true
	apply(t, s, event, Unchanged)

	event.Description = "300ml"
	apply(t, s, event, Updated)
	row = db.KnowledgeBaseEntry.GetX(ctx, id)
	if row.Embedded || row.Content != "Shea Lotion\n300ml\nPrice: 850.00" {
		t.Fatalf("an event updates the words, keeps the price, and queues embedding: %q embedded=%v", row.Content, row.Embedded)
	}
}

// An event that cannot prove an item is for sale neither creates an entry nor revives a stale one.
func TestSyncUpdateOnly(t *testing.T) {
	db, tid := openSyncDB(t)
	s := NewSync(db, nil)
	ctx := context.Background()
	it := SourceItem{TenantID: tid, Source: SourceInventory, SourceID: "i2", Kind: "GOODS", Name: "Flour", Sellable: true, UpdateOnly: true}
	apply(t, s, it, Skipped)
	if db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.TenantID(tid)).CountX(ctx) != 0 {
		t.Fatal("update only creates nothing")
	}
	it.UpdateOnly = false
	apply(t, s, it, Created)
	gone := it
	gone.Sellable = false
	apply(t, s, gone, MadeStale)
	id := EntryID(tid, SourceInventory, "i2")
	if r := db.KnowledgeBaseEntry.GetX(ctx, id); !r.Embedded || r.ChunkCount != 0 || r.Metadata["stale"] != true {
		t.Fatalf("stale leaves nothing to embed: %+v", r)
	}
	it.UpdateOnly = true
	apply(t, s, it, Unchanged)
	it.UpdateOnly = false
	apply(t, s, it, Updated)
	if r := db.KnowledgeBaseEntry.GetX(ctx, id); r.Embedded || r.Metadata["stale"] != nil {
		t.Fatalf("the walk brings it back and queues embedding: %+v", r)
	}
}

// The owner's words are never overwritten, whether they edited a synced entry or wrote their own.
func TestSyncRespectsTheOwner(t *testing.T) {
	db, tid := openSyncDB(t)
	s := NewSync(db, nil)
	ctx := context.Background()
	svc := &Service{client: db}
	it := SourceItem{TenantID: tid, Source: SourceInventory, SourceID: "i3", Kind: "SERVICE", Name: "Haircut", Description: "Basic", Sellable: true}
	apply(t, s, it, Created)
	id := EntryID(tid, SourceInventory, "i3")
	// The portal form sends every field on each save: saving unchanged words edits nothing.
	before := db.KnowledgeBaseEntry.GetX(ctx, id)
	db.KnowledgeBaseEntry.UpdateOneID(id).SetEmbedded(true).ExecX(ctx)
	same, title := before.Content, before.Title
	if _, err := svc.UpdateEntry(ctx, tid, id, UpdateEntryInput{Content: &same, Title: &title, Tags: []string{"hair"}}); err != nil {
		t.Fatal(err)
	}
	if r := db.KnowledgeBaseEntry.GetX(ctx, id); r.Metadata["owner_edited"] != nil || !r.Embedded || r.Metadata["source_id"] != "i3" {
		t.Fatalf("a tag fix is not an edit of the words: %+v", r)
	}
	mine := "Our signature cut, by Mary"
	if _, err := svc.UpdateEntry(ctx, tid, id, UpdateEntryInput{Content: &mine}); err != nil {
		t.Fatal(err)
	}
	it.Description = "Basic cut and wash"
	apply(t, s, it, Flagged)
	r := db.KnowledgeBaseEntry.GetX(ctx, id)
	if r.Content != mine || r.Metadata["may_be_outdated"] != true || r.Metadata["source_id"] != "i3" {
		t.Fatalf("flagged, not overwritten, tracking kept: %+v", r)
	}
	apply(t, s, it, Unchanged) // flagged once for one change

	// A hand-typed entry with the same name gets a flag when the price differs, and no rival.
	hand := db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetCategory("product").SetTitle("Blue Soap").SetContent("My soap").SetProductPrice(100).SaveX(ctx)
	apply(t, s, SourceItem{TenantID: tid, Source: SourceInventory, SourceID: "i4", Kind: "GOODS", Name: "blue soap", Price: price(120), Sellable: true}, Flagged)
	if got := db.KnowledgeBaseEntry.GetX(ctx, hand.ID); got.Content != "My soap" || got.Metadata["may_be_outdated"] != true {
		t.Fatalf("hand entry: %+v", got)
	}
	if db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.TenantID(tid), knowledgebaseentry.TitleEqualFold("blue soap")).CountX(ctx) != 1 {
		t.Fatal("no rival entry")
	}
	// Answering the flag by editing clears it.
	price := 120.0
	if _, err := svc.UpdateEntry(ctx, tid, hand.ID, UpdateEntryInput{ProductPrice: &price}); err != nil {
		t.Fatal(err)
	}
	if got := db.KnowledgeBaseEntry.GetX(ctx, hand.ID); got.Metadata["may_be_outdated"] != nil {
		t.Fatalf("saving answers the flag: %+v", got.Metadata)
	}
}

// Items the walk did not see go stale; an empty walk changes nothing; other sources are untouched.
func TestMarkMissing(t *testing.T) {
	db, tid := openSyncDB(t)
	s := NewSync(db, nil)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		apply(t, s, SourceItem{TenantID: tid, Source: SourceInventory, SourceID: id, Kind: "GOODS", Name: "Item " + id, Sellable: true}, Created)
	}
	apply(t, s, SourceItem{TenantID: tid, Source: SourceWebsite, SourceID: "w", Kind: "COURSE", Name: "Coding", Sellable: true}, Created)
	if n, err := s.MarkMissing(ctx, tid, SourceInventory, map[string]bool{}); err != nil || n != 0 {
		t.Fatalf("an empty walk proves nothing: %d %v", n, err)
	}
	if n, err := s.MarkMissing(ctx, tid, SourceInventory, map[string]bool{"a": true}); err != nil || n != 1 {
		t.Fatalf("one missing: %d %v", n, err)
	}
	if db.KnowledgeBaseEntry.GetX(ctx, EntryID(tid, SourceInventory, "b")).Metadata["stale"] != true ||
		db.KnowledgeBaseEntry.GetX(ctx, EntryID(tid, SourceInventory, "a")).Metadata["stale"] != nil ||
		db.KnowledgeBaseEntry.GetX(ctx, EntryID(tid, SourceWebsite, "w")).Metadata["stale"] != nil {
		t.Fatal("only the unseen item of this source goes stale")
	}
}
