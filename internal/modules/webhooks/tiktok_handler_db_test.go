package webhooks

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

// TikTok lead webhooks carry only the advertiser id: the tenant is whoever connected that
// advertiser (connected_accounts), a revoked link does not count, an advertiser two tenants
// both connected is refused rather than guessed, and one TikTok lead id maps to one lead.
func TestTikTokTenantLookupAndDedupe(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWebhooksPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	defer sqlDB.Close()
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer db.Close()
	ctx := context.Background()

	tid, other, third := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{tid, other, third} {
		if _, err := db.Tenant.Create().SetID(id).SetName("TT " + id.String()).SetSlug("tt-" + id.String()).Save(ctx); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, id := range []uuid.UUID{tid, other, third} {
			_, _ = db.Lead.Delete().Where(lead.TenantID(id)).Exec(ctx)
			_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(id)).Exec(ctx)
			_ = db.Tenant.DeleteOneID(id).Exec(ctx)
		}
	})
	adv, shared, revoked := "ADV-"+tid.String()[:8], "ADV-S-"+tid.String()[:8], "ADV-R-"+tid.String()[:8]
	link := func(tenant uuid.UUID, ext, status string) {
		if _, err := db.ConnectedAccount.Create().SetTenantID(tenant).SetPlatform("tiktok").SetAccountType("tiktok_advertiser").
			SetExternalID(ext).SetStatus(status).Save(ctx); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	link(tid, adv, "active")
	link(tid, shared, "active")
	link(other, shared, "expired")
	link(third, revoked, "revoked")

	h := NewTikTokHandler(nil, TikTokConfig{DB: sqlDB}, zap.NewNop())

	got, err := h.resolveTenantByAdvertiserID(ctx, adv)
	if err != nil || got != tid {
		t.Fatalf("resolve = %v, %v; want %v", got, err, tid)
	}
	if _, err := h.resolveTenantByAdvertiserID(ctx, "ADV-unknown"); err == nil {
		t.Fatal("an unknown advertiser must not resolve")
	}
	if _, err := h.resolveTenantByAdvertiserID(ctx, shared); err == nil {
		t.Fatal("an advertiser two tenants connected must be refused, not guessed")
	}
	if _, err := h.resolveTenantByAdvertiserID(ctx, revoked); err == nil {
		t.Fatal("a revoked link must not resolve")
	}

	if h.leadExists(ctx, tid, "TL-1") {
		t.Fatal("no lead yet")
	}
	if _, err := db.Lead.Create().SetTenantID(tid).SetSource("tiktok").
		SetMetadata(map[string]any{"tiktok_lead_id": "TL-1"}).Save(ctx); err != nil {
		t.Fatalf("seed lead: %v", err)
	}
	if !h.leadExists(ctx, tid, "TL-1") {
		t.Fatal("the stored TikTok lead id must be found")
	}
	if h.leadExists(ctx, other, "TL-1") {
		t.Fatal("dedupe must stay inside the tenant")
	}
}
