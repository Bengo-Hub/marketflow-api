package webhooks

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/migrate"
)

// One comment is one Activity on redelivery, the same person is found again by platform id
// (the jsonb @> lookup over the GIN index), and another tenant never sees it.
func TestInboxStoreDedupeAndContactMatch(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWebhooksPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	defer sqlDB.Close()
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer db.Close()
	ctx := context.Background()
	if err := db.Schema.Create(ctx, schema.WithDir(migrate.Dir)); err != nil {
		t.Fatalf("schema create (checks the new inbox indexes): %v", err)
	}

	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Inbox " + tid.String()).SetSlug("inbox-" + tid.String()).ExecX(ctx)
	page := "PAGE-" + tid.String()[:8]
	db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("meta").SetAccountType("facebook_page").
		SetExternalID(page).SetDisplayName("Test Page").ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.Activity.Delete().Where(activity.TenantID(tid)).Exec(ctx)
		_, _ = db.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
	})

	s := &InboxStore{DB: db, Logger: zap.NewNop()}
	at := time.Now().UTC().Truncate(time.Second)
	c1 := InboxItem{Platform: "facebook", AccountID: page, Kind: "comment", ExternalID: "C-" + tid.String(), PostID: page + "_P1",
		SenderID: "U-" + tid.String(), SenderName: "Amina", Text: "How much is this?", At: at}
	if n := s.Store(ctx, []InboxItem{c1, c1}); n != 2 {
		t.Fatalf("both handled (one stored, one skipped as a redelivery), got %d", n)
	}
	acts := db.Activity.Query().Where(activity.TenantID(tid)).AllX(ctx)
	if len(acts) != 1 {
		t.Fatalf("one comment is one activity, got %d", len(acts))
	}
	a := acts[0]
	if a.ActivityType != "comment" || a.Source == nil || *a.Source != "facebook" || a.Metadata["intent_status"] != "pending" ||
		a.Metadata["thread_id"] != "facebook:"+page+"_P1" || a.EntityType != "contact" {
		t.Fatalf("activity: %+v", a)
	}

	// The same person sends a DM: found by platform id, not duplicated.
	dm := InboxItem{Platform: "facebook", AccountID: page, Kind: "dm", ExternalID: "M-" + tid.String(),
		SenderID: c1.SenderID, Text: "Do you deliver?", At: at}
	s.Store(ctx, []InboxItem{dm})
	if n := db.Contact.Query().Where(contact.TenantID(tid)).CountX(ctx); n != 1 {
		t.Fatalf("the commenter must be matched, got %d contacts", n)
	}
	ct := db.Contact.Query().Where(contact.TenantID(tid)).OnlyX(ctx)
	if ct.SubscribedEmail || ct.SubscribedSms || ct.ContactType != "prospect" {
		t.Fatalf("a social contact starts with consent off: %+v", ct)
	}

	// An unknown account stores nothing.
	if n := s.Store(ctx, []InboxItem{{Platform: "facebook", AccountID: "NOPE", Kind: "dm", ExternalID: "x", SenderID: "y", Text: "hi"}}); n != 0 {
		t.Fatalf("no tenant, no item: %d", n)
	}
}
