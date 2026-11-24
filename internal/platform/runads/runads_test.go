package runads

import (
	"testing"
)

func TestMergeTenantOverrides(t *testing.T) {
	p := Fallback()
	p.RuleOfThumbKES = map[string]Tiers{Meta: {TierLow: 1, TierRecommended: 2, TierHigh: 3}}
	s := Merge(p, map[string]any{TenantKey: map[string]any{
		"platforms": []any{"google", "meta", "bogus"}, "tier": "high", "days": float64(14),
		"daily_budget": map[string]any{"meta": float64(1500), "tiktok": float64(-1)}, "total_cap": float64(30000),
	}})
	if len(s.Platforms) != 2 || s.Platforms[0] != Meta || s.Platforms[1] != Google {
		t.Fatalf("platforms = %v, want [meta google] in plan order", s.Platforms)
	}
	if zero := Merge(p, map[string]any{TenantKey: map[string]any{"total_cap": float64(0)}}); !zero.TotalCapSet || zero.TotalCap != 0 {
		t.Fatal("a tenant cap of 0 must read as set (no cap)")
	}
	if s.Tier != TierHigh || s.Days != 14 || s.TotalCap != 30000 {
		t.Fatalf("tier/days/cap = %s/%d/%d", s.Tier, s.Days, s.TotalCap)
	}
	if s.DailyBudget[Meta] != 1500 || s.DailyBudget[TikTok] != 0 {
		t.Fatalf("daily budget = %v", s.DailyBudget)
	}
	if len(p.Platforms) != 3 {
		t.Fatal("merge changed the platform settings")
	}
	// An explicit empty list switches every platform off.
	if s := Merge(p, map[string]any{TenantKey: map[string]any{"platforms": []any{}}}); len(s.Platforms) != 0 {
		t.Fatalf("empty list = %v", s.Platforms)
	}
}

func TestValidate(t *testing.T) {
	ok := map[string]any{TenantKey: map[string]any{"platforms": []any{"TikTok", "meta"}, "days": float64(7),
		"daily_budget": map[string]any{"google": float64(0), "meta": float64(900)}}}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	got := ok[TenantKey].(map[string]any)
	if p := got["platforms"].([]string); len(p) != 2 || p[0] != Meta || p[1] != TikTok {
		t.Fatalf("platforms = %v", p)
	}
	if b := got["daily_budget"].(map[string]any); len(b) != 1 || b["meta"] != int64(900) {
		t.Fatalf("daily_budget = %v", b)
	}
	for _, bad := range []map[string]any{
		{"days": float64(0)}, {"days": float64(91)}, {"tier": "max"}, {"platforms": []any{"x"}},
		{"daily_budget": map[string]any{"meta": 1.5}}, {"total_cap": float64(-1)}, {"budget": 1},
	} {
		if err := Validate(map[string]any{TenantKey: bad}); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := Validate(map[string]any{TenantKey: nil}); err != nil {
		t.Fatal("null must clear the override")
	}
}

func TestChanged(t *testing.T) {
	stored := map[string]any{TenantKey: map[string]any{"days": float64(7)}}
	if Changed(stored, map[string]any{"days": int64(7)}) {
		t.Fatal("same value read as a change")
	}
	if !Changed(stored, map[string]any{"days": int64(8)}) {
		t.Fatal("a new value not read as a change")
	}
}

func TestParseRuleOfThumb(t *testing.T) {
	r, ok := ParseRuleOfThumb(`{"meta":{"low":650,"recommended":1300,"high":2600},"google":{"low":5,"recommended":1,"high":9},"x":{"low":1,"recommended":2,"high":3}}`)
	if !ok || len(r) != 1 || r[Meta][TierRecommended] != 1300 {
		t.Fatalf("rule of thumb = %v (descending google and unknown x must be dropped)", r)
	}
	if _, ok := ParseRuleOfThumb(""); ok {
		t.Fatal("empty value read as a rule of thumb")
	}
}

func TestBuildTiersRuleOfThumbWithPastResults(t *testing.T) {
	tiers := BuildTiers(TierInput{Days: 10, RuleOfThumb: Tiers{TierLow: 500, TierRecommended: 1000, TierHigh: 2000},
		Past: &Past{Spend: 5000, Clicks: 100, Leads: 10}})
	if len(tiers) != 3 || tiers[1].Name != TierRecommended || !tiers[1].Recommended || tiers[1].Source != SourceRuleOfThumb {
		t.Fatalf("tiers = %+v", tiers)
	}
	// 1000 a day for 10 days at a past cost per lead of 500 and per click of 50.
	est := tiers[1].Estimates
	if len(est) != 2 || est[0].Metric != "leads" || est[0].Value != 20 || est[1].Value != 200 || est[0].Source != SourceYourResults {
		t.Fatalf("estimates = %+v", est)
	}
}

func TestBuildTiersNoSourceNoTiers(t *testing.T) {
	if tiers := BuildTiers(TierInput{Days: 7}); len(tiers) != 0 {
		t.Fatalf("tiers without a source = %+v", tiers)
	}
}

func TestBuildTiersPlatformOptions(t *testing.T) {
	tiers := BuildTiers(TierInput{Days: 7, OptionsLabel: "Google", Options: []Option{
		{Daily: 3000, Estimates: []Estimate{{Metric: "clicks", Value: 90, Per: "week"}}},
		{Daily: 1000}, {Daily: 2000, Recommended: true}, {Daily: 1500},
	}})
	if len(tiers) != 3 || tiers[0].Daily != 1000 || tiers[1].Daily != 2000 || tiers[2].Daily != 3000 || tiers[2].Estimates[0].Value != 90 {
		t.Fatalf("tiers = %+v", tiers)
	}
	if one := BuildTiers(TierInput{Options: []Option{{Daily: 700}}}); len(one) != 1 {
		t.Fatalf("one option made %d tiers", len(one))
	}
}

func TestCurveIsNotExtrapolated(t *testing.T) {
	in := TierInput{Curve: []CurvePoint{{Spend: 100, Reach: 1000, Actions: 2}, {Spend: 300, Reach: 2000, Actions: 4}},
		CurveLabel: "Meta", ActionMetric: "conversations"}
	est := EstimatesFor(200, in)
	if len(est) != 2 || est[0].Value != 1500 || est[1].Value != 3 {
		t.Fatalf("estimates at 200 = %+v", est)
	}
	if est := EstimatesFor(301, in); len(est) != 0 {
		t.Fatalf("extrapolated past the curve: %+v", est)
	}
}

func TestPick(t *testing.T) {
	tiers := []Tier{{Name: TierLow, Daily: 1}, {Name: TierRecommended, Daily: 2, Recommended: true}}
	if d, n := Pick(tiers, TierHigh, 0); d != 2 || n != TierRecommended {
		t.Fatalf("missing tier picked %d %s", d, n)
	}
	if d, n := Pick(tiers, TierLow, 9); d != 9 || n != TierCustom {
		t.Fatalf("own amount picked %d %s", d, n)
	}
	if d, _ := Pick(nil, TierLow, 0); d != 0 {
		t.Fatal("no tiers must pick 0")
	}
}

func TestLinks(t *testing.T) {
	s := Fallback()
	if got := s.TikTokLink("payment", "7123"); got != "https://ads.tiktok.com/i18n/account/payment?aadvid=7123" {
		t.Fatalf("payment link = %s", got)
	}
	if got := s.TikTokLink("payment", ""); got != "https://ads.tiktok.com/" {
		t.Fatalf("payment link without advertiser = %s", got)
	}
	s.GoogleBilling = "https://ads.google.com/aw/billing/summary?__e={customer_id}"
	if got := s.GoogleBillingLink("123-456-7890"); got != "https://ads.google.com/aw/billing/summary?__e=1234567890" {
		t.Fatalf("billing link = %s", got)
	}
	if got := Fallback().GoogleBillingLink("1"); got != DefaultGoogleBillingURL {
		t.Fatalf("default billing link = %s", got)
	}
}

func TestMedianDays(t *testing.T) {
	if MedianDays([]int{30, 7, 90}) != 30 || MedianDays(nil) != 0 {
		t.Fatal("median")
	}
}
