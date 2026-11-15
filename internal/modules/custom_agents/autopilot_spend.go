package custom_agents

import (
	"context"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/google/uuid"
)

// The monthly ad ceiling (Sprint 20): a paid agent's monthly_ad_ceiling is checked against the
// real spend of the boosts that run its videos, read from the stored daily ad metrics the hourly
// insights pass writes. The paid run checks it before it makes anything, and the boost wizard
// checks it before a boost of an agent's video is queued.

// briefOfAgent matches the briefs an autopilot agent made (metadata.autopilot_agent_id).
func briefOfAgent(agentID uuid.UUID) predicate.CreativeBrief {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString(s.C(creativebrief.FieldMetadata) + "->>'autopilot_agent_id' = ").Arg(agentID.String())
		}))
	}
}

// monthStart is the 1st of now's month, UTC, the day the ad metrics rows are dated in.
func monthStart(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// AdSpendThisMonth sums the real ad spend since the 1st (UTC) of every boost that runs a video
// the agent made. A video made last month and boosted this month counts, so the briefs are read
// without a date bound; only the metrics rows are bounded to the month. Every query is scoped to
// the tenant.
func AdSpendThisMonth(ctx context.Context, db *ent.Client, tenantID, agentID uuid.UUID, now time.Time) (float64, error) {
	briefs, err := db.CreativeBrief.Query().Where(creativebrief.TenantID(tenantID), briefOfAgent(agentID)).IDs(ctx)
	if err != nil || len(briefs) == 0 {
		return 0, err
	}
	versions, err := db.CreativeVersion.Query().
		Where(creativeversion.TenantID(tenantID), creativeversion.BriefIDIn(briefs...)).IDs(ctx)
	if err != nil || len(versions) == 0 {
		return 0, err
	}
	boosts, err := db.Campaign.Query().
		Where(campaign.TenantIDEQ(tenantID), campaign.CreativeVersionIDIn(versions...)).IDs(ctx)
	if err != nil || len(boosts) == 0 {
		return 0, err
	}
	spends, err := db.AdMetrics.Query().
		Where(admetrics.TenantID(tenantID), admetrics.CampaignIDIn(boosts...), admetrics.DateGTE(monthStart(now))).
		Select(admetrics.FieldSpend).Float64s(ctx)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, s := range spends {
		total += s
	}
	return total, nil
}

// AdCeilingCheck says whether an ad of budget (whole units) fits the agent's monthly ad ceiling on
// top of this month's real spend, and if not, why in plain words. An agent that is not paid
// always fits and nothing is read. spent is the month's spend in whole units, rounded up.
func AdCeilingCheck(ctx context.Context, db *ent.Client, a *ent.CustomAgent, cfg AutopilotConfig, budget int64, now time.Time) (ok bool, why string, spent int64, err error) {
	if !cfg.Paid {
		return true, "", 0, nil
	}
	total, err := AdSpendThisMonth(ctx, db, a.TenantID, a.ID, now)
	if err != nil {
		return false, "", 0, err
	}
	spent = SpendUnits(total)
	if cfg.FitsMonth(spent, budget) {
		return true, "", spent, nil
	}
	return false, fmt.Sprintf("its ads have spent %d this month, and another %d would pass the monthly ad ceiling of %d",
		spent, budget, cfg.MonthlyAdCap), spent, nil
}

// AgentOfVersion follows a creative version to its brief and the autopilot agent that made it.
// It returns nil and no error when the version was not made by an agent, or its agent is gone; an
// error only when the database could not be read.
func AgentOfVersion(ctx context.Context, db *ent.Client, tenantID, versionID uuid.UUID) (*ent.CustomAgent, error) {
	v, err := db.CreativeVersion.Query().
		Where(creativeversion.IDEQ(versionID), creativeversion.TenantIDEQ(tenantID)).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := db.CreativeBrief.Query().
		Where(creativebrief.IDEQ(v.BriefID), creativebrief.TenantIDEQ(tenantID)).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw, _ := b.Metadata["autopilot_agent_id"].(string)
	agentID, err := uuid.Parse(raw)
	if err != nil {
		return nil, nil
	}
	ag, err := db.CustomAgent.Query().
		Where(customagent.IDEQ(agentID), customagent.TenantIDEQ(tenantID)).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return ag, err
}
