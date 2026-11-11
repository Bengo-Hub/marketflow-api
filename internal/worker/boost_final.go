package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
)

// finalSpendDue reports whether a boost's final spend should be recorded now: its end date has
// passed (plus a day for Meta's late reporting) and it was not recorded before (Sprint 18 L43).
func finalSpendDue(c *ent.Campaign, now time.Time) bool {
	if c.EndDate == nil || now.Before(c.EndDate.Add(24*time.Hour)) {
		return false
	}
	_, done := c.Metadata["final_spend"]
	return !done
}

// recordFinalSpend stores a finished boost's lifetime spend and results in its metadata, once.
func (a *AdInsights) recordFinalSpend(ctx context.Context, c *ent.Campaign) {
	if !finalSpendDue(c, time.Now()) {
		return
	}
	var totals []struct {
		Spend   float64 `json:"sum_spend"`
		Leads   int64   `json:"sum_leads"`
		Convers int64   `json:"sum_conversions"`
	}
	if err := a.db.AdMetrics.Query().Where(admetrics.CampaignID(c.ID)).
		Aggregate(ent.As(ent.Sum(admetrics.FieldSpend), "sum_spend"), ent.As(ent.Sum(admetrics.FieldLeads), "sum_leads"),
			ent.As(ent.Sum(admetrics.FieldConversions), "sum_conversions")).
		Scan(ctx, &totals); err != nil || len(totals) == 0 {
		return
	}
	meta := map[string]any{}
	for k, v := range c.Metadata {
		meta[k] = v
	}
	meta["final_spend"] = totals[0].Spend
	meta["final_results"] = totals[0].Leads + totals[0].Convers
	meta["ended_at"] = c.EndDate.UTC().Format(time.RFC3339)
	// Status stays as it is: "completed" means deleted for campaigns, and the boost should stay listed.
	if err := a.db.Campaign.UpdateOneID(c.ID).SetMetadata(meta).Exec(ctx); err != nil {
		a.logger.Warn("final spend not saved", zap.String("campaign", c.ID.String()), zap.Error(err))
	}
}
