package webhooks

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
)

// A page two tenants connected is refused (logged and dropped); a page of one tenant stores.
func TestInboxStorePageConflict(t *testing.T) {
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

	newTenant := func() uuid.UUID {
		tid := uuid.New()
		db.Tenant.Create().SetID(tid).SetName("Inbox " + tid.String()).SetSlug("inbox-" + tid.String()).ExecX(ctx)
		t.Cleanup(func() {
			_, _ = db.Activity.Delete().Where(activity.TenantID(tid)).Exec(ctx)
			_, _ = db.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
			_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
			_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
		})
		return tid
	}
	connect := func(tid uuid.UUID, page string) {
		db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("meta").SetAccountType("facebook_page").
			SetExternalID(page).SetDisplayName("Page").ExecX(ctx)
	}
	a, b := newTenant(), newTenant()
	shared := "PAGE-SHARED-" + a.String()[:8]
	own := "PAGE-OWN-" + a.String()[:8]
	connect(a, shared)
	connect(b, shared)
	connect(a, own)

	s := &InboxStore{DB: db, Logger: zap.NewNop()}
	item := func(page, id string) InboxItem {
		return InboxItem{Platform: "facebook", AccountID: page, Kind: "dm", ExternalID: id, SenderID: "U-" + id, Text: "hi", At: time.Now().UTC()}
	}
	cases := []struct {
		name string
		it   InboxItem
		want int
	}{
		{"page of two tenants is dropped", item(shared, "S-"+a.String()), 0},
		{"page of one tenant stores", item(own, "O-"+a.String()), 1},
	}
	for _, c := range cases {
		if n := s.Store(ctx, []InboxItem{c.it}); n != c.want {
			t.Errorf("%s: stored %d, want %d", c.name, n, c.want)
		}
	}
	for _, tid := range []uuid.UUID{a, b} {
		if n := db.Activity.Query().Where(activity.TenantID(tid), activity.ExternalRef("S-"+a.String())).CountX(ctx); n != 0 {
			t.Fatalf("the shared page's item reached tenant %s", tid)
		}
	}
}
