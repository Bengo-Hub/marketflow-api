package worker

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/adsmanager"
)

// optimiserMetaKey is the campaign metadata key holding the optimiser's pauses, newest last.
const optimiserMetaKey = "optimiser"

// OptimiserNotice tells the owner which ads of a boost were paused, with an undo link.
type OptimiserNotice func(ctx context.Context, c *ent.Campaign, pausedAdIDs []string)

// WithOptimiser turns on the budget optimiser (Sprint 18 L38; away decision 23) for boosts with
// extra creatives.
func (a *AdInsights) WithOptimiser(notify OptimiserNotice) *AdInsights {
	a.optimiserNotice = notify
	a.optimiserOn = true
	return a
}

// boostLaunchState reads a boost's launch state from campaign metadata.
func boostLaunchState(c *ent.Campaign) (adsmanager.State, bool) {
	raw, ok := c.Metadata["launch"]
	if !ok {
		return adsmanager.State{}, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return adsmanager.State{}, false
	}
	var l struct {
		State adsmanager.State `json:"state"`
	}
	if json.Unmarshal(b, &l) != nil {
		return adsmanager.State{}, false
	}
	return l.State, true
}

// alreadyPausedByOptimiser lists the ads the optimiser paused before.
func alreadyPausedByOptimiser(c *ent.Campaign) map[string]bool {
	out := map[string]bool{}
	if o, ok := c.Metadata[optimiserMetaKey].(map[string]any); ok {
		if list, ok := o["paused"].([]any); ok {
			for _, e := range list {
				if m, ok := e.(map[string]any); ok {
					if id, ok := m["ad_id"].(string); ok {
						out[id] = true
					}
				}
			}
		}
	}
	return out
}

// optimise pauses a live boost's clear losers. The ads share one ad-set lifetime budget, so the
// money moves to the remaining ads without changing the approved total or end date.
func (a *AdInsights) optimise(ctx context.Context, c *ent.Campaign, token string, minSpend float64) {
	st, ok := boostLaunchState(c)
	if !ok || st.Stage != adsmanager.StageLive || len(st.Extras) == 0 || st.AdSetID == "" || st.LiveAt.IsZero() {
		return
	}
	rows, err := a.meta.GetAdLifetimeInsights(ctx, token, st.AdSetID)
	if err != nil {
		a.logger.Debug("optimiser: ad insights skipped", zap.String("campaign", c.ID.String()), zap.Error(err))
		return
	}
	done := alreadyPausedByOptimiser(c)
	var ads []adsmanager.AdResult
	for _, r := range rows {
		if r.AdID == "" || done[r.AdID] {
			continue
		}
		leads, conv := r.Results()
		ads = append(ads, adsmanager.AdResult{AdID: r.AdID, Spend: parseFloat(r.Spend), Results: leads + conv})
	}
	losers := adsmanager.OptimiserLosers(ads, time.Since(st.LiveAt), minSpend)
	if len(losers) == 0 {
		return
	}
	var paused []string
	for _, id := range losers {
		if err := a.meta.SetStatus(ctx, token, id, "PAUSED"); err != nil {
			a.logger.Warn("optimiser: pause failed", zap.String("ad", id), zap.Error(err))
			continue
		}
		paused = append(paused, id)
	}
	if len(paused) == 0 {
		return
	}
	meta := map[string]any{}
	for k, v := range c.Metadata {
		meta[k] = v
	}
	o, _ := meta[optimiserMetaKey].(map[string]any)
	if o == nil {
		o = map[string]any{}
	}
	list, _ := o["paused"].([]any)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range paused {
		list = append(list, map[string]any{"ad_id": id, "at": now, "reason": "cost per result at least twice the best ad's"})
	}
	o["paused"] = list
	meta[optimiserMetaKey] = o
	if err := a.db.Campaign.UpdateOneID(c.ID).SetMetadata(meta).Exec(ctx); err != nil {
		a.logger.Warn("optimiser: log not saved", zap.String("campaign", c.ID.String()), zap.Error(err))
	}
	a.logger.Info("optimiser paused ads", zap.String("campaign", c.ID.String()), zap.Strings("ads", paused))
	if a.optimiserNotice != nil {
		a.optimiserNotice(ctx, c, paused)
	}
}
