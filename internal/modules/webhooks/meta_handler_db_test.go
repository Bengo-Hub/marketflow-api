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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

const testWebhooksPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

// Meta redelivers leadgen webhooks and sends numeric campaign ids: one leadgen id must map to
// one lead, and the campaign must be found by meta_campaign_id.
func TestMetaLeadgenDedupeAndCampaignLookup(t *testing.T) {
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

	tid, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{tid, other} {
		if _, err := db.Tenant.Create().SetID(id).SetName("Hook " + id.String()).SetSlug("hook-" + id.String()).Save(ctx); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	// Cleanup touches only this test's own tenant ids.
	t.Cleanup(func() {
		for _, id := range []uuid.UUID{tid, other} {
			_, _ = db.Lead.Delete().Where(lead.TenantID(id)).Exec(ctx)
			_, _ = db.Campaign.Delete().Where(campaign.TenantID(id)).Exec(ctx)
			_ = db.Tenant.DeleteOneID(id).Exec(ctx)
		}
	})

	h := NewMetaHandler(nil, MetaConfig{DB: sqlDB}, zap.NewNop())

	if h.leadExists(ctx, tid, "LG-9") {
		t.Fatal("no lead yet")
	}
	if _, err := db.Lead.Create().SetTenantID(tid).SetSource("meta_ad").SetLeadType("customer").
		SetMetadata(map[string]any{"meta_leadgen_id": "LG-9"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.leadExists(ctx, tid, "LG-9") {
		t.Fatal("a stored leadgen id must be detected as a redelivery")
	}
	if h.leadExists(ctx, other, "LG-9") {
		t.Fatal("dedupe must be per tenant")
	}
	if h.leadExists(ctx, tid, "") {
		t.Fatal("an empty leadgen id never matches")
	}

	c, err := db.Campaign.Create().SetTenantID(tid).SetName("C9 leads").SetMetaCampaignID("120211234567890").Save(ctx)
	if err != nil {
		t.Fatalf("seed campaign: %v", err)
	}
	if got, ok := h.campaignByMetaID(ctx, tid, "120211234567890"); !ok || got != c.ID {
		t.Fatalf("campaign lookup = %s, %v; want %s", got, ok, c.ID)
	}
	if _, ok := h.campaignByMetaID(ctx, other, "120211234567890"); ok {
		t.Fatal("campaign lookup must be per tenant")
	}
	if _, ok := h.campaignByMetaID(ctx, tid, "999"); ok {
		t.Fatal("unknown Meta campaign must not match")
	}
}
