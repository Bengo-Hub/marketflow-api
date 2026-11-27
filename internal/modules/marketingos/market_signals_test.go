package marketingos

import (
	"encoding/json"
	"fmt"
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
	// Configured competitor names pass through, capped, for the "What has worked before" suggestion.
	names := make([]any, maxSignalCompetitors+3)
	for i := range names {
		names[i] = fmt.Sprintf("Rival %d", i)
	}
	if c := marketSignals(map[string]any{CompetitorsKey: names}); len(c.Competitors) != maxSignalCompetitors || c.Empty() {
		t.Fatalf("competitor names: %+v", c.Competitors)
	}
}

// Tagged winners add their labels to the pattern, and the scan's summary comes along.
func TestMarketSignalsTagsAndSummary(t *testing.T) {
	var meta map[string]any
	_ = json.Unmarshal([]byte(`{"competitor_scan": {"summary": "Long ads open with a question.", "competitors": [{"query": "rival.co.ke", "winners": [
		{"advertiser": "Rival Ltd", "format": "video", "total_days_shown": 40, "tags": {"hook_type": "social_proof", "angle": "pain", "offer_type": "free_delivery", "relevance": "related"}}
	]}]}}`), &meta)
	s := marketSignals(meta)
	want := "video ad that ran 40 days; hook: social proof; angle: pain; offer: free delivery; relevance: related"
	if len(s.Patterns) != 1 || s.Patterns[0] != want {
		t.Fatalf("pattern with tags:\n got %v\nwant %q", s.Patterns, want)
	}
	if s.Summary != "Long ads open with a question." {
		t.Fatalf("summary passes through: %q", s.Summary)
	}
	var noWinners map[string]any
	_ = json.Unmarshal([]byte(`{"competitor_scan": {"summary": "x", "competitors": []}}`), &noWinners)
	if marketSignals(noWinners).Summary != "" {
		t.Fatal("a summary without winners is dropped")
	}
}

func TestScanBrand(t *testing.T) {
	meta := map[string]any{MetadataKey: map[string]any{"positioning": "Fresh cakes in Nakuru", "audience": []any{"parents"}, "offers": []any{"Birthday cakes", "Cupcakes"}}}
	if got := ScanBrand(meta); got != "Fresh cakes in Nakuru. Audience: parents. Offers: Birthday cakes, Cupcakes" {
		t.Fatalf("brand line: %q", got)
	}
	if ScanBrand(map[string]any{}) != "" {
		t.Fatal("no brand context, no line")
	}
	long := map[string]any{MetadataKey: map[string]any{"positioning": strings.Repeat("a", 2000)}}
	if n := len([]rune(ScanBrand(long))); n != maxScanBrand {
		t.Fatalf("the line is capped at %d runes, got %d", maxScanBrand, n)
	}
}
