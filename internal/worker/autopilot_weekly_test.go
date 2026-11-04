package worker

import (
	"context"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"go.uber.org/zap"
)

// The week counts only this agent's ads and only the last 7 days of metrics.
func TestAutopilotWeekSummary(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "weekly")
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.AdMetrics.Delete().Where(admetrics.TenantID(tid)).Exec(ctx)
		_, _ = db.Campaign.Delete().Where(campaign.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	a := db.CustomAgent.Create().SetTenantID(tid).SetName("Weekly ads").SetTriggerType(custom_agents.TriggerSchedule).SetIsActive(true).
		SetMetadata(map[string]any{"kind": custom_agents.KindAdAutopilot}).SaveX(ctx)
	mine := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Mine").SetMetadata(map[string]any{"autopilot_agent_id": a.ID.String()}).SaveX(ctx)
	other := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Other").SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(mine.ID).SetVersionNo(1).SetFormat("1080x1920").SetCredits(12).SaveX(ctx)
	db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(other.ID).SetVersionNo(1).SetFormat("1080x1920").SetCredits(99).ExecX(ctx)
	c := db.Campaign.Create().SetTenantID(tid).SetName("Boost").SetCampaignType("lead_gen").SetPlatform("meta").SetStatus("active").
		SetCreativeVersionID(v.ID).SaveX(ctx)
	now := time.Now().UTC()
	day := func(ago int, spend float64, leads int64) {
		db.AdMetrics.Create().SetTenantID(tid).SetCampaignID(c.ID).SetPlatform("meta").SetExternalCampaignID("m").
			SetDate(now.AddDate(0, 0, -ago)).SetSpend(spend).SetLeads(leads).SetCurrency("KES").ExecX(ctx)
	}
	day(1, 600, 2)
	day(3, 400, 3)
	day(20, 9999, 50) // outside the week

	w := NewAutopilotWeekly(db, nil, "https://mf.example", zap.NewNop())
	s, err := w.summary(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if s.AdsMade != 1 || s.Credits != 12 || s.Spend != 1000 || s.Leads != 5 || s.CPL != 200 {
		t.Fatalf("summary: %+v", s)
	}
}
