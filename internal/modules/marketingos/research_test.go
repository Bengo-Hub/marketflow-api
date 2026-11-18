package marketingos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

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

func TestMarketResearchShowsTagsButNotOddValues(t *testing.T) {
	meta := map[string]any{CompetitorScanKey: map[string]any{"competitors": []any{
		map[string]any{"query": "acme", "winners": []any{
			map[string]any{"format": "video", "total_days_shown": 92, "tags": map[string]any{
				"hook_type": "question", "angle": "pain", "offer_type": "<script>", "relevance": "very relevant"}},
			map[string]any{"format": "image", "total_days_shown": 10},
		}},
	}}}
	got := marketResearch(meta)[0].Winners
	if got[0].Tags == nil || got[0].Tags.HookType != "question" || got[0].Tags.Relevance != "very relevant" {
		t.Fatalf("tags reach the owner: %+v", got[0].Tags)
	}
	if got[0].Tags.OfferType != "" {
		t.Fatalf("a value that is not a label is dropped: %q", got[0].Tags.OfferType)
	}
	if got[1].Tags != nil {
		t.Fatal("an untagged winner has no tags")
	}
}

func TestScanQuotaReader(t *testing.T) {
	var gotKey, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotTenant = r.Header.Get("X-API-Key"), r.URL.Query().Get("tenant_id")
		_, _ = w.Write([]byte(`{"month":"2026-11","scan":{"tenant_used":2,"tenant_limit":5,"tenant_left":3},"details":{"tenant_left":1}}`))
	}))
	defer srv.Close()
	tid := uuid.New()
	q, err := NewScanQuotaReader(srv.URL, "k")(context.Background(), tid)
	if err != nil || q.Scan.TenantLeft != 3 || q.Details.TenantLeft != 1 || q.Month != "2026-11" {
		t.Fatalf("quota read: %+v %v", q, err)
	}
	if gotKey != "k" || gotTenant != tid.String() {
		t.Fatalf("service key and tenant sent: %q %q", gotKey, gotTenant)
	}
	if NewScanQuotaReader("", "k") != nil {
		t.Fatal("no marketflow-ai, no reader")
	}
}

func TestMarketResearchWithoutAScanIsEmptyNotNil(t *testing.T) {
	if got := marketResearch(map[string]any{}); got == nil || len(got) != 0 {
		t.Fatalf("research = %#v", got)
	}
}
