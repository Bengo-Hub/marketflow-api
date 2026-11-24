package runads

import (
	"math"
	"sort"
)

// Where a tier amount or an estimate comes from. The plan never shows a number without one.
const (
	// SourcePlatform: the ad platform's own recommendation or estimate (Label names it).
	SourcePlatform = "platform"
	// SourceYourResults: the tenant's own past results on that platform (stored ad metrics).
	SourceYourResults = "your_results"
	// SourceRuleOfThumb: MarketFlow's rule of thumb from platform config; no estimate behind it.
	SourceRuleOfThumb = "rule_of_thumb"
	// SourceCompetitors: the tenant's competitor ad scan.
	SourceCompetitors = "competitors"
)

// Estimate is what a budget buys, with where the number came from.
type Estimate struct {
	// Metric is reach, impressions, clicks, conversions or leads.
	Metric string `json:"metric"`
	Value  int64  `json:"value"`
	// Per is day, week or run (the whole plan).
	Per    string `json:"per"`
	Source string `json:"source"`
	Label  string `json:"label"`
}

// Tier is one budget choice for a platform. Daily is in the tenant's currency.
type Tier struct {
	Name        string     `json:"name"`
	Daily       int64      `json:"daily"`
	Recommended bool       `json:"recommended,omitempty"`
	Source      string     `json:"source"`
	Label       string     `json:"label"`
	Estimates   []Estimate `json:"estimates"`
}

// Option is one budget the platform itself recommended (Google Ads budget options), daily in the
// tenant's currency, with what the platform estimates it buys.
type Option struct {
	Daily       int64
	Recommended bool
	Estimates   []Estimate
}

// CurvePoint is one point of Meta's delivery estimate: daily spend (tenant currency) and what
// Meta estimates it reaches and gets.
type CurvePoint struct {
	Spend   float64
	Reach   float64
	Actions float64
}

// Past is the tenant's own results on a platform (tenant currency).
type Past struct {
	Spend       float64
	Impressions int64
	Clicks      int64
	Leads       int64
}

// TierInput is everything known about one platform's budgets.
type TierInput struct {
	Days         int
	Options      []Option
	OptionsLabel string
	// RuleOfThumb is in the tenant's currency.
	RuleOfThumb Tiers
	Curve       []CurvePoint
	CurveLabel  string
	// ActionMetric names what Meta's curve counts as an action (leads, clicks, conversations).
	ActionMetric string
	Past         *Past
}

// Labels shown with a source.
const (
	LabelRuleOfThumb = "MarketFlow rule of thumb (platform settings), not an estimate"
	LabelYourResults = "From your own past results on this platform"
)

// BuildTiers makes up to three tiers (low, recommended, high). Amounts come from the platform's own
// options, else the rule of thumb; with neither there are no tiers and the owner types an amount.
// Each tier then gets the estimates there are a source for: the platform's own, Meta's delivery
// curve, or the tenant's past cost per result. Nothing is extrapolated past the data.
func BuildTiers(in TierInput) []Tier {
	var tiers []Tier
	switch {
	case len(in.Options) > 0:
		tiers = fromOptions(in.Options, in.OptionsLabel)
	case len(in.RuleOfThumb) == 3:
		for _, n := range []string{TierLow, TierRecommended, TierHigh} {
			tiers = append(tiers, Tier{Name: n, Daily: in.RuleOfThumb[n], Recommended: n == TierRecommended,
				Source: SourceRuleOfThumb, Label: LabelRuleOfThumb, Estimates: []Estimate{}})
		}
	default:
		return []Tier{}
	}
	for i := range tiers {
		if len(tiers[i].Estimates) > 0 {
			continue
		}
		tiers[i].Estimates = EstimatesFor(tiers[i].Daily, in)
	}
	return tiers
}

// EstimatesFor is what a daily amount buys from the curve, else from past results.
func EstimatesFor(daily int64, in TierInput) []Estimate {
	out := []Estimate{}
	if reach, actions, ok := onCurve(in.Curve, float64(daily)); ok {
		if reach > 0 {
			out = append(out, Estimate{Metric: "reach", Value: int64(math.Round(reach)), Per: "day", Source: SourcePlatform, Label: in.CurveLabel})
		}
		if actions > 0 && in.ActionMetric != "" {
			out = append(out, Estimate{Metric: in.ActionMetric, Value: int64(math.Round(actions)), Per: "day", Source: SourcePlatform, Label: in.CurveLabel})
		}
		if len(out) > 0 {
			return out
		}
	}
	if p := in.Past; p != nil && p.Spend > 0 && in.Days > 0 {
		total := float64(daily) * float64(in.Days)
		add := func(metric string, n int64) {
			if n > 0 {
				out = append(out, Estimate{Metric: metric, Value: int64(math.Floor(total / (p.Spend / float64(n)))), Per: "run",
					Source: SourceYourResults, Label: LabelYourResults})
			}
		}
		add("leads", p.Leads)
		add("clicks", p.Clicks)
	}
	return out
}

func fromOptions(opts []Option, label string) []Tier {
	sorted := append([]Option(nil), opts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Daily < sorted[j].Daily })
	rec := -1
	for i, o := range sorted {
		if o.Recommended {
			rec = i
			break
		}
	}
	if rec < 0 {
		rec = len(sorted) / 2
	}
	picks := []struct {
		name string
		idx  int
	}{{TierLow, 0}, {TierRecommended, rec}, {TierHigh, len(sorted) - 1}}
	var out []Tier
	used := map[int]bool{}
	for _, p := range picks {
		if used[p.idx] {
			continue
		}
		used[p.idx] = true
		o := sorted[p.idx]
		est := append([]Estimate{}, o.Estimates...)
		out = append(out, Tier{Name: p.name, Daily: o.Daily, Recommended: p.name == TierRecommended,
			Source: SourcePlatform, Label: label, Estimates: est})
	}
	return out
}

// onCurve interpolates the curve at spend; ok is false without data or past its last point.
func onCurve(curve []CurvePoint, spend float64) (reach, actions float64, ok bool) {
	pts := make([]CurvePoint, 0, len(curve)+1)
	for _, p := range curve {
		if p.Spend > 0 {
			pts = append(pts, p)
		}
	}
	if len(pts) == 0 || spend <= 0 {
		return 0, 0, false
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].Spend < pts[j].Spend })
	if spend < pts[0].Spend || spend > pts[len(pts)-1].Spend {
		return 0, 0, false
	}
	if spend == pts[0].Spend {
		return pts[0].Reach, pts[0].Actions, true
	}
	prev := pts[0]
	for _, p := range pts[1:] {
		if spend <= p.Spend && p.Spend > prev.Spend {
			f := (spend - prev.Spend) / (p.Spend - prev.Spend)
			return prev.Reach + f*(p.Reach-prev.Reach), prev.Actions + f*(p.Actions-prev.Actions), true
		}
		prev = p
	}
	return 0, 0, false
}

// Pick is the daily amount for a platform: the tenant's own amount, else the named tier, else the
// recommended tier; 0 when there is none (the owner types one).
func Pick(tiers []Tier, tier string, own int64) (int64, string) {
	if own > 0 {
		return own, TierCustom
	}
	for _, t := range tiers {
		if t.Name == tier {
			return t.Daily, t.Name
		}
	}
	for _, t := range tiers {
		if t.Recommended {
			return t.Daily, t.Name
		}
	}
	return 0, TierCustom
}

// MedianDays is the median run length of the competitor ads still running (0 when none).
func MedianDays(days []int) int {
	if len(days) == 0 {
		return 0
	}
	d := append([]int(nil), days...)
	sort.Ints(d)
	return d[len(d)/2]
}
