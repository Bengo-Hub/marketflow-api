package admin

import (
	"context"
	"testing"
	"time"
)

// The breakdowns of one month: by feature, by day, by route (a video ad's own scenes, reused ones
// left out), the six-month trend, and the platform owner's own spend. The month is in 2002 so no
// other test's data falls inside it.
func TestCostBreakdowns(t *testing.T) {
	db, sqlDB := openCostReportDB(t)
	ctx := context.Background()
	_, start, end, err := monthRange("2002-05", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	owner, briefO := costTenant(t, db, "Platform owner")
	shop, briefS := costTenant(t, db, "Shop")
	ownerSlug := db.Tenant.GetX(ctx, owner).Slug
	no := 0
	day := func(d int) time.Time { return time.Date(2002, 5, d, 9, 0, 0, 0, time.UTC) }
	mk := func(tid, brief [16]byte, kind string, credits int, at time.Time, meta map[string]any) {
		no++
		db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(brief).SetVersionNo(no).SetKind(kind).SetFormat("1920x1080").
			SetStatus("ready").SetCredits(credits).SetCreatedAt(at).SetMetadata(meta).ExecX(ctx)
	}
	mk(shop, briefS, "poster", 2, day(3), map[string]any{"cost_usd": 0.04, "route": "fal/seedream-v5-lite"})
	mk(shop, briefS, "video_ad", 30, day(3), map[string]any{"cost_usd": 1.0, "scene_cache": []any{
		map[string]any{"route": "fal/creatify-boreal", "cost_usd": 0.05},
		map[string]any{"route": "fal/hailuo-2.3-fast", "cost_usd": 0.19},
		map[string]any{"route": "fal/hailuo-2.3-fast", "cost_usd": 0.19, "reused": true},
	}})
	mk(owner, briefO, "video_ad", 0, day(9), map[string]any{"cost_usd": 1.77})
	// The month before, for the trend.
	mk(shop, briefS, "poster", 2, time.Date(2002, 4, 20, 9, 0, 0, 0, time.UTC), map[string]any{"cost_usd": 0.04})

	bd, err := queryCostBreakdowns(ctx, sqlDB, start, end, ownerSlug)
	if err != nil {
		t.Fatal(err)
	}
	if len(bd.ByFeature) != 2 || bd.ByFeature[0].Key != "video_ad" || bd.ByFeature[0].Versions != 2 || !near(bd.ByFeature[0].CostUSD, 2.77) {
		t.Fatalf("by feature %+v", bd.ByFeature)
	}
	if len(bd.Daily) != 2 || bd.Daily[0].Key != "2002-05-03" || bd.Daily[0].Credits != 32 {
		t.Fatalf("daily %+v", bd.Daily)
	}
	routes := map[string]float64{}
	for _, r := range bd.ByRoute {
		routes[r.Key] = r.CostUSD
	}
	if !near(routes["fal/hailuo-2.3-fast"], 0.19) || !near(routes["fal/creatify-boreal"], 0.05) || !near(routes["fal/seedream-v5-lite"], 0.04) {
		t.Fatalf("by route %+v (a reused scene must not count)", bd.ByRoute)
	}
	if len(bd.Monthly) != 2 || bd.Monthly[0].Key != "2002-04" {
		t.Fatalf("monthly %+v", bd.Monthly)
	}
	if bd.Owner == nil || bd.Owner.Versions != 1 || bd.Owner.Credits != 0 || !near(bd.Owner.CostUSD, 1.77) {
		t.Fatalf("platform owner %+v", bd.Owner)
	}
}
