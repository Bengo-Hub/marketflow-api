package marketingos

import "testing"

func TestMarketResearchKeepsOnlyTransparencyLinks(t *testing.T) {
	meta := map[string]any{CompetitorScanKey: map[string]any{"competitors": []any{
		map[string]any{"query": "acme", "read_at": "2026-11-02T00:00:00Z", "winners": []any{
			map[string]any{"advertiser": "Acme", "format": "video", "total_days_shown": 92, "still_running": true,
				"details_link": "https://adstransparency.google.com/advertiser/AR1/creative/CR1"},
			map[string]any{"advertiser": "Acme", "format": "text", "total_days_shown": 40,
				"details_link": "https://serpapi.com/search.json?engine=google_ads_transparency_center_ad_details"},
		}},
	}}}
	got := marketResearch(meta)
	if len(got) != 1 || got[0].Query != "acme" || len(got[0].Winners) != 2 {
		t.Fatalf("research = %+v", got)
	}
	if w := got[0].Winners[0]; w.Link == "" || w.DaysShown != 92 || !w.StillRunning {
		t.Fatalf("first winner = %+v", w)
	}
	if got[0].Winners[1].Link != "" {
		t.Fatalf("a provider link reached the owner: %q", got[0].Winners[1].Link)
	}
}

func TestMarketResearchWithoutAScanIsEmptyNotNil(t *testing.T) {
	if got := marketResearch(map[string]any{}); got == nil || len(got) != 0 {
		t.Fatalf("research = %#v", got)
	}
}
