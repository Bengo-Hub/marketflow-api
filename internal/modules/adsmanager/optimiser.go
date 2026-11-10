package adsmanager

import "time"

// Budget optimiser (Sprint 18 L38; owner 2026-11-11: conservative rules, away decision 23). The
// ads of one boost share one ad-set lifetime budget, so pausing a clear loser moves its share to
// the others without ever exceeding the approved total or end date.
const (
	// OptimiserMinLive is how long a boost runs before any ad is judged.
	OptimiserMinLive = 48 * time.Hour
	// OptimiserLoserFactor: an ad whose cost per result is this many times the best ad's is a loser.
	OptimiserLoserFactor = 2.0
)

// AdResult is one ad's lifetime spend and results (leads or conversations).
type AdResult struct {
	AdID    string
	Spend   float64
	Results int64
}

// OptimiserLosers returns the ads to pause: once the boost has run OptimiserMinLive and spent
// minSpend in total, an ad with a cost per result at least OptimiserLoserFactor times the best
// ad's, or with no result after spending OptimiserLoserFactor times the best cost per result.
// Nothing is judged without a winner (an ad with results), and at least one ad keeps running.
func OptimiserLosers(ads []AdResult, liveFor time.Duration, minSpend float64) []string {
	if len(ads) < 2 || liveFor < OptimiserMinLive {
		return nil
	}
	var total float64
	best := -1.0
	for _, a := range ads {
		total += a.Spend
		if a.Results > 0 {
			if cpr := a.Spend / float64(a.Results); best < 0 || cpr < best {
				best = cpr
			}
		}
	}
	if total < minSpend || best <= 0 {
		return nil
	}
	var losers []string
	for _, a := range ads {
		switch {
		case a.Results > 0 && a.Spend/float64(a.Results) >= OptimiserLoserFactor*best:
			losers = append(losers, a.AdID)
		case a.Results == 0 && a.Spend >= OptimiserLoserFactor*best:
			losers = append(losers, a.AdID)
		}
	}
	if len(losers) >= len(ads) {
		return nil // never pause every ad
	}
	return losers
}
