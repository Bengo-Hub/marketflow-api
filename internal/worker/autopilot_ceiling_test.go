package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/adsmanager"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A paid run checks the monthly ad ceiling against this month's real spend of the agent's boosts
// (the stored ad metrics) before it reads the ad account or makes anything: past the ceiling it
// is skipped as ad_ceiling; last month's spend and boosts of other videos do not count.
func TestAutopilotRunAdCeiling(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "autopilot-ceiling")
	t.Cleanup(func() {
		_, _ = db.AdMetrics.Delete().Where(admetrics.TenantID(tid)).Exec(ctx)
		_, _ = db.Campaign.Delete().Where(campaign.TenantID(tid)).Exec(ctx)
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	a := db.CustomAgent.Create().SetTenantID(tid).SetName("Paid ads").SetTriggerType(custom_agents.TriggerSchedule).
		SetIsActive(true).SetMetadata(map[string]any{
		"kind": custom_agents.KindAdAutopilot,
		"autopilot": map[string]any{"offering_ids": []string{"o1"}, "objective": "whatsapp", "cadence": "daily", "time": "09:00",
			"platforms": []string{"facebook"}, "connected_account_ids": []string{uuid.NewString()}, "monthly_credit_ceiling": 100,
			"paid": true, "per_ad_budget": 2000, "period_days": 7, "monthly_ad_ceiling": 5000},
		"autopilot_offerings": []map[string]any{{"name": "Blender", "photo_urls": []string{"https://media.example/blender.jpg"}}},
	}).SaveX(ctx)

	// A video the agent made last month, boosted; and a hand-made video's boost.
	brief := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Blender").SetMetadata(map[string]any{"autopilot_agent_id": a.ID.String()}).
		SetCreatedAt(time.Now().AddDate(0, -1, 0)).SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(brief.ID).SetVersionNo(1).SetFormat("1080x1920").SaveX(ctx)
	other := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Hand made").SaveX(ctx)
	ov := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(other.ID).SetVersionNo(1).SetFormat("1080x1920").SaveX(ctx)
	boost := func(version uuid.UUID) *ent.Campaign {
		return db.Campaign.Create().SetTenantID(tid).SetName("Boost").SetCampaignType("lead_gen").SetPlatform("meta").
			SetStatus("active").SetCurrency("KES").SetConnectedAccountID(uuid.New()).SetCreativeVersionID(version).SaveX(ctx)
	}
	now := time.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	day := func(c *ent.Campaign, date time.Time, spend float64) {
		db.AdMetrics.Create().SetTenantID(tid).SetCampaignID(c.ID).SetPlatform("meta").SetDate(date).SetSpend(spend).SetCurrency("KES").ExecX(ctx)
	}
	mine, hand := boost(v.ID), boost(ov.ID)
	day(mine, thisMonth.AddDate(0, 0, -1), 9000) // last month
	day(mine, thisMonth, 3000.4)                 // rounds up to 3001: 3001 + 2000 passes 5000
	day(hand, thisMonth, 9000)                   // not the agent's video

	funds := 0
	r := NewAutopilotRunner(db, nil, "https://mf.example", zap.NewNop()).
		WithAdFunds(func(context.Context, uuid.UUID, []string, int64, int) (adsmanager.AccountFunds, error) {
			funds++
			return adsmanager.AccountFunds{ConnectedID: uuid.NewString()}, nil
		})
	if _, err := r.Run(ctx, a); !errors.Is(err, errAutopilotAdCeiling) || !errors.Is(err, ErrAutopilotSkipped) {
		t.Fatalf("past the monthly ad ceiling must skip: %v", err)
	}
	if funds != 0 {
		t.Fatal("the ceiling is checked before the ad account is read")
	}
	if n := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).CountX(ctx); n != 2 {
		t.Fatalf("a skipped run made a brief: %d", n)
	}

	// Exactly at the ceiling fits.
	db.AdMetrics.Update().Where(admetrics.CampaignID(mine.ID), admetrics.DateEQ(thisMonth)).SetSpend(3000).ExecX(ctx)
	res, err := r.Run(ctx, a)
	if err != nil || funds != 1 {
		t.Fatalf("within the ceiling the run goes on to the ad account check and the draft: %+v %v", res, err)
	}
	b := db.CreativeBrief.GetX(ctx, uuid.MustParse(res.BriefID))
	if got, _ := b.Metadata["ad_spent_this_month"].(float64); got != 3000 {
		t.Fatalf("the brief records the month's spend it saw: %v", b.Metadata["ad_spent_this_month"])
	}
}

// AgentOfVersion follows a video to the agent that made it, and finds none for a hand-made one.
func TestAgentOfVersion(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "agent-of-version")
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	a := seedAutopilot(t, db, tid, 100)
	brief := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Blender").SetMetadata(map[string]any{"autopilot_agent_id": a.ID.String()}).SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(brief.ID).SetVersionNo(1).SetFormat("1080x1920").SaveX(ctx)
	other := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Hand made").SaveX(ctx)
	ov := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(other.ID).SetVersionNo(1).SetFormat("1080x1920").SaveX(ctx)

	if got, err := custom_agents.AgentOfVersion(ctx, db, tid, v.ID); err != nil || got == nil || got.ID != a.ID {
		t.Fatalf("agent's video: %v %v", got, err)
	}
	if got, err := custom_agents.AgentOfVersion(ctx, db, tid, ov.ID); err != nil || got != nil {
		t.Fatalf("hand-made video: %v %v", got, err)
	}
	if got, err := custom_agents.AgentOfVersion(ctx, db, uuid.New(), v.ID); err != nil || got != nil {
		t.Fatalf("another tenant never sees the agent: %v %v", got, err)
	}
}

// A run after the agent's end date makes nothing and is skipped as ended.
func TestAutopilotRunEnded(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "autopilot-ended")
	t.Cleanup(func() {
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	a := seedAutopilot(t, db, tid, 100)
	md := a.Metadata
	ap, _ := md["autopilot"].(map[string]any)
	ap["end_date"] = time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	a = db.CustomAgent.UpdateOneID(a.ID).SetMetadata(md).SaveX(ctx)
	r := NewAutopilotRunner(db, nil, "https://mf.example", zap.NewNop())
	if _, err := r.Run(ctx, a); !errors.Is(err, errAutopilotEnded) {
		t.Fatalf("past the end date must skip: %v", err)
	}
	if n := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).CountX(ctx); n != 0 {
		t.Fatalf("an ended agent made %d briefs", n)
	}
}
