package marketingos

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCleanReferenceAds(t *testing.T) {
	got, err := cleanReferenceAds([]ReferenceAd{
		{URL: " https://www.facebook.com/ads/library/?id=1 ", Platform: "Meta", Notes: " problem hook, then demo "},
		{URL: "https://www.facebook.com/ads/library/?id=1", Platform: "meta"}, // duplicate link
		{}, // empty row
		{URL: "https://tiktok.com/@shop/video/2"}, // platform defaults to other
	})
	if err != nil || len(got) != 2 || got[0].Platform != "meta" || got[0].Notes != "problem hook, then demo" || got[1].Platform != "other" {
		t.Fatalf("trimmed, deduped, defaulted: %+v %v", got, err)
	}
	for name, bad := range map[string]ReferenceAd{
		"not a link":     {URL: "facebook ad with the lady"},
		"script scheme":  {URL: "javascript:alert(1)"},
		"unknown place":  {URL: "https://x.example", Platform: "myspace"},
		"notes too long": {URL: "https://x.example", Notes: strings.Repeat("a", MaxReferenceNotes+1)},
	} {
		if _, err := cleanReferenceAds([]ReferenceAd{bad}); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	many := make([]ReferenceAd, MaxReferenceAds+1)
	for i := range many {
		many[i] = ReferenceAd{URL: "https://x.example/" + strings.Repeat("a", i+1)}
	}
	if _, err := cleanReferenceAds(many); err == nil {
		t.Fatal("more than the limit must be refused")
	}
}

func TestCleanComplaints(t *testing.T) {
	got, err := cleanComplaints([]string{" Slow delivery ", "slow delivery", "", "No reply on WhatsApp"})
	if err != nil || len(got) != 2 {
		t.Fatalf("trimmed and deduped: %v %v", got, err)
	}
	if _, err := cleanComplaints([]string{strings.Repeat("a", MaxComplaintText+1)}); err == nil {
		t.Fatal("a long complaint must be refused")
	}
}

// Signals describe winners by shape only: no advertiser name or ad text reaches the writer.
func TestMarketSignalsShapeOnly(t *testing.T) {
	var meta map[string]any
	_ = json.Unmarshal([]byte(`{
		"competitor_scan": {"competitors": [{"query": "rival.co.ke", "winners": [
			{"advertiser": "Rival Ltd", "format": "VIDEO", "total_days_shown": 92, "still_running": true, "details_link": "https://adstransparency.google.com/x"},
			{"advertiser": "Rival Ltd", "format": "image", "total_days_shown": 0}
		]}]},
		"reference_ads": [{"url": "https://www.facebook.com/ads/library/?id=1", "platform": "meta", "notes": "mother talks to camera"}],
		"customer_complaints": ["Slow delivery"]
	}`), &meta)
	s := marketSignals(meta)
	if len(s.Patterns) != 1 || s.Patterns[0] != "video ad that ran 92 days, still running" {
		t.Fatalf("one pattern by shape: %v", s.Patterns)
	}
	b, _ := json.Marshal(s)
	for _, leak := range []string{"Rival", "rival.co.ke", "adstransparency"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("signals must not carry %q: %s", leak, b)
		}
	}
	if len(s.References) != 1 || len(s.Complaints) != 1 || s.Empty() {
		t.Fatalf("references and complaints pass through: %+v", s)
	}
	if !marketSignals(map[string]any{}).Empty() {
		t.Fatal("no data means no signals")
	}
}
