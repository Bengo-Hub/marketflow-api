package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestCPLStopReason(t *testing.T) {
	stop := autopilotStop{Agent: "Weekly ads", Limit: 300, Days: 3}
	if r := cplStopReason(stop, 400, 0, 500, "KES"); r != "" {
		t.Fatalf("judged under the minimum spend: %q", r)
	}
	if r := cplStopReason(stop, 900, 3, 500, "KES"); r != "" {
		t.Fatalf("300 a lead is at the limit, not above it: %q", r)
	}
	if r := cplStopReason(stop, 1200, 3, 500, "KES"); !strings.Contains(r, "400 KES") || !strings.Contains(r, "last 3 days") {
		t.Fatalf("above the limit must pause: %q", r)
	}
	if r := cplStopReason(stop, 550, 0, 500, "KES"); r != "" {
		t.Fatalf("no leads pauses only after twice the limit: %q", r)
	}
	if r := cplStopReason(stop, 600, 0, 500, "KES"); !strings.Contains(r, "no leads") {
		t.Fatalf("no leads after twice the limit must pause: %q", r)
	}
}

// A boost of an autopilot's video pauses on the agent's rule over its window only, after a full
// window live, and boosts of other videos are left to the tenant's own ceiling.
func TestAutopilotCPLStop(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "autopilot-cpl")
	t.Cleanup(func() {
		_, _ = db.AdMetrics.Delete().Where(admetrics.TenantID(tid)).Exec(ctx)
		_, _ = db.Campaign.Delete().Where(campaign.TenantID(tid)).Exec(ctx)
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
	})
	ag := db.CustomAgent.Create().SetTenantID(tid).SetName("Weekly ads").SetTriggerType(custom_agents.TriggerSchedule).
		SetIsActive(false).SetMetadata(map[string]any{
		"kind":      custom_agents.KindAdAutopilot,
		"autopilot": map[string]any{"stop_cpl": 300.0, "stop_cpl_days": 2},
	}).SaveX(ctx)
	brief := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Blender").SetMetadata(map[string]any{"autopilot_agent_id": ag.ID.String()}).SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(brief.ID).SetVersionNo(1).SetFormat("1080x1920").SaveX(ctx)
	other := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Hand made").SaveX(ctx)
	ov := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(other.ID).SetVersionNo(1).SetFormat("1080x1920").SaveX(ctx)

	now := time.Now().UTC()
	boost := func(version uuid.UUID, liveAgo time.Duration, mc string) *ent.Campaign {
		return db.Campaign.Create().SetTenantID(tid).SetName("Boost " + mc).SetCampaignType("lead_gen").SetPlatform("meta").
			SetStatus("active").SetCurrency("KES").SetConnectedAccountID(uuid.New()).SetMetaCampaignID(mc).
			SetCreativeVersionID(version).
			SetMetadata(map[string]any{"launch": map[string]any{"state": map[string]any{"stage": "live", "live_at": now.Add(-liveAgo).Format(time.RFC3339)}}}).
			SaveX(ctx)
	}
	day := func(c *ent.Campaign, daysAgo int, spend float64, leads int64) {
		db.AdMetrics.Create().SetTenantID(tid).SetCampaignID(c.ID).SetPlatform("meta").SetExternalCampaignID(*c.MetaCampaignID).
			SetDate(now.Truncate(24*time.Hour).AddDate(0, 0, -daysAgo)).SetSpend(spend).SetLeads(leads).SetCurrency("KES").ExecX(ctx)
	}
	var paused []string
	a := NewAdInsights(db, time.Hour, nil, zap.NewNop()).WithBoosts(nil, func(_ context.Context, c *ent.Campaign, r string) error {
		paused = append(paused, *c.MetaCampaignID+": "+r)
		return nil
	})

	young := boost(v.ID, 24*time.Hour, "young") // one day live, the window is two
	day(young, 0, 2000, 1)
	a.autoPause(ctx, db.Campaign.GetX(ctx, young.ID))

	mine := boost(v.ID, 5*24*time.Hour, "mine")
	day(mine, 4, 5000, 0) // outside the window: an old bad day does not count
	day(mine, 1, 300, 1)
	day(mine, 0, 300, 1)
	a.autoPause(ctx, db.Campaign.GetX(ctx, mine.ID))
	if len(paused) != 0 {
		t.Fatalf("paused too early or on an old day: %v", paused)
	}
	// Today's row moves on: the window now has 1,100 for 2 leads, 550 each.
	db.AdMetrics.Update().Where(admetrics.CampaignID(mine.ID), admetrics.DateEQ(now.Truncate(24*time.Hour))).SetSpend(800).SetLeads(1).ExecX(ctx)
	a.autoPause(ctx, db.Campaign.GetX(ctx, mine.ID))
	if len(paused) != 1 || !strings.Contains(paused[0], "mine") || !strings.Contains(paused[0], "550 KES") {
		t.Fatalf("must pause above the agent's limit: %v", paused)
	}

	hand := boost(ov.ID, 5*24*time.Hour, "hand")
	day(hand, 0, 5000, 1)
	a.autoPause(ctx, db.Campaign.GetX(ctx, hand.ID))
	if len(paused) != 1 {
		t.Fatalf("a boost of a video no agent made follows only the tenant's ceiling: %v", paused)
	}
}
