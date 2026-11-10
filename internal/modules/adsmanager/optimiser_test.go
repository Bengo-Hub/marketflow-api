package adsmanager

import (
	"testing"
	"time"
)

func TestOptimiserLosers(t *testing.T) {
	ads := []AdResult{
		{AdID: "win", Spend: 1000, Results: 10},  // 100 per lead
		{AdID: "ok", Spend: 900, Results: 6},     // 150: kept
		{AdID: "lose", Spend: 1200, Results: 5},  // 240: >= 2x best
		{AdID: "silent", Spend: 250, Results: 0}, // no lead after 2.5x best
	}
	got := OptimiserLosers(ads, 72*time.Hour, 500)
	if len(got) != 2 || got[0] != "lose" || got[1] != "silent" {
		t.Fatalf("losers: %v", got)
	}
	if OptimiserLosers(ads, 24*time.Hour, 500) != nil {
		t.Error("nothing is judged before 48 hours")
	}
	if OptimiserLosers(ads, 72*time.Hour, 10000) != nil {
		t.Error("nothing is judged before the minimum spend")
	}
	if OptimiserLosers([]AdResult{{AdID: "a", Spend: 900}, {AdID: "b", Spend: 900}}, 72*time.Hour, 500) != nil {
		t.Error("no winner, nothing to judge against")
	}
	if OptimiserLosers([]AdResult{{AdID: "a", Spend: 900, Results: 9}}, 72*time.Hour, 500) != nil {
		t.Error("a single ad is never paused")
	}
}

// The undo endpoint only switches on an ad of this boost.
func TestBoostOwnsAd(t *testing.T) {
	st := State{AdID: "ad1", Extras: []ExtraState{{AdID: "ad2"}}}
	if !boostOwnsAd(st, "ad1") || !boostOwnsAd(st, "ad2") || boostOwnsAd(st, "ad9") || boostOwnsAd(st, "") {
		t.Fatal("ownership")
	}
}
