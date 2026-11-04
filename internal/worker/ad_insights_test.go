package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Auto-pause judges a boost only after the tenant's minimum spend, pauses above the CPL ceiling,
// and never twice.
func TestBoostAutoPause(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "autopause")
	t.Cleanup(func() {
		_, _ = db.AdMetrics.Delete().Where(admetrics.TenantID(tid)).Exec(ctx)
		_, _ = db.Campaign.Delete().Where(campaign.TenantID(tid)).Exec(ctx)
	})
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{"ads_max_cpl": 200.0, "ads_min_spend_to_judge": 1000.0}).ExecX(ctx)
	acc := uuid.New()
	c := db.Campaign.Create().SetTenantID(tid).SetName("Boost").SetCampaignType("lead_gen").SetPlatform("meta").
		SetStatus("active").SetCurrency("KES").SetConnectedAccountID(acc).SetMetaCampaignID("mc1").SaveX(ctx)
	day := func(d int, spend float64, leads int64) {
		db.AdMetrics.Create().SetTenantID(tid).SetCampaignID(c.ID).SetPlatform("meta").SetExternalCampaignID("mc1").
			SetDate(time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC)).SetSpend(spend).SetLeads(leads).SetCurrency("KES").ExecX(ctx)
	}
	var reasons []string
	a := NewAdInsights(db, time.Hour, nil, zap.NewNop()).WithBoosts(nil, func(_ context.Context, _ *ent.Campaign, r string) error {
		reasons = append(reasons, r)
		return nil
	})
	day(1, 600, 1) // 600 spent: under the 1,000 needed to judge
	a.autoPause(ctx, db.Campaign.GetX(ctx, c.ID))
	if len(reasons) != 0 {
		t.Fatalf("judged too early: %v", reasons)
	}
	day(2, 900, 2) // 1,500 for 3 leads = 500 each, above 200
	a.autoPause(ctx, db.Campaign.GetX(ctx, c.ID))
	if len(reasons) != 1 || !strings.Contains(reasons[0], "500") {
		t.Fatalf("must pause above the ceiling: %v", reasons)
	}
	db.Campaign.UpdateOneID(c.ID).SetMetadata(map[string]any{"auto_paused_at": "x"}).ExecX(ctx)
	a.autoPause(ctx, db.Campaign.GetX(ctx, c.ID))
	if len(reasons) != 1 {
		t.Fatal("a boost is auto-paused once")
	}
}
