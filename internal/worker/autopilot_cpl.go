package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
)

// The autopilot CPL stop (Sprint 20): a boost that runs a video an ad_autopilot agent made is
// paused when its cost per lead over the agent's window (stop_cpl_days, default 3) is above the
// agent's stop_cpl. It runs inside the hourly insights pass, after the tenant-wide ceiling, and
// pauses through the same BoostOps.Pause: paused on Meta, marked auto_paused_at, the admins
// emailed once. A boost the owner resumes after that is not paused again by either rule.

// autopilotStop is the stop rule of the agent whose video the boost runs.
type autopilotStop struct {
	Agent string
	Limit float64
	Days  int
}

// autopilotStopFor follows the boost's creative version to its brief and the agent that made it.
// Any missing link means the boost is not an autopilot's and no agent rule applies. The rule
// holds while the agent is switched off too: its ads may still be running.
func (a *AdInsights) autopilotStopFor(ctx context.Context, c *ent.Campaign) (autopilotStop, bool) {
	if c.CreativeVersionID == nil {
		return autopilotStop{}, false
	}
	ag, err := custom_agents.AgentOfVersion(ctx, a.db, c.TenantID, *c.CreativeVersionID)
	if err != nil || ag == nil {
		return autopilotStop{}, false
	}
	cfg, _, err := custom_agents.ReadAutopilot(ag)
	if err != nil || cfg.StopCPL <= 0 {
		return autopilotStop{}, false
	}
	return autopilotStop{Agent: ag.Name, Limit: cfg.StopCPL, Days: cfg.StopWindow()}, true
}

// autopilotCPLReason judges the boost on the agent's rule; "" when it holds, when no agent rule
// applies, or when the boost has not run a full window yet (so Meta's learning days never count
// as a breach).
func (a *AdInsights) autopilotCPLReason(ctx context.Context, c *ent.Campaign, minSpend float64, now time.Time) string {
	stop, ok := a.autopilotStopFor(ctx, c)
	if !ok {
		return ""
	}
	since := c.CreatedAt
	if st, ok := boostLaunchState(c); ok && !st.LiveAt.IsZero() {
		since = st.LiveAt
	}
	if now.Sub(since) < time.Duration(stop.Days)*24*time.Hour {
		return ""
	}
	today := now.UTC().Truncate(24 * time.Hour)
	var totals []struct {
		Spend   float64 `json:"sum_spend"`
		Leads   int64   `json:"sum_leads"`
		Convers int64   `json:"sum_conversions"`
	}
	if err := a.db.AdMetrics.Query().
		Where(admetrics.CampaignID(c.ID), admetrics.DateGTE(today.AddDate(0, 0, -(stop.Days-1)))).
		Aggregate(ent.As(ent.Sum(admetrics.FieldSpend), "sum_spend"), ent.As(ent.Sum(admetrics.FieldLeads), "sum_leads"),
			ent.As(ent.Sum(admetrics.FieldConversions), "sum_conversions")).
		Scan(ctx, &totals); err != nil || len(totals) == 0 {
		return ""
	}
	t := totals[0]
	return cplStopReason(stop, t.Spend, t.Leads+t.Convers, minSpend, c.Currency)
}

// cplStopReason is the rule itself: judged only once the window's spend reaches minSpend; above
// the limit per lead, or no lead at all after twice the limit, pauses.
func cplStopReason(stop autopilotStop, spend float64, results int64, minSpend float64, currency string) string {
	if spend < minSpend {
		return ""
	}
	window := fmt.Sprintf("the last %d days", stop.Days)
	if stop.Days == 1 {
		window = "today"
	}
	switch {
	case results == 0 && spend >= 2*stop.Limit:
		return fmt.Sprintf("it spent %.0f %s over %s with no leads, and your agent \"%s\" stops its ads above %.0f %s a lead",
			spend, currency, window, stop.Agent, stop.Limit, currency)
	case results > 0 && spend/float64(results) > stop.Limit:
		return fmt.Sprintf("each lead cost %.0f %s over %s, above the limit of %.0f %s your agent \"%s\" keeps to",
			spend/float64(results), currency, window, stop.Limit, currency, stop.Agent)
	}
	return ""
}
