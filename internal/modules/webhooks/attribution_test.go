package webhooks

import (
	"strings"
	"testing"
)

// Sprint 18 L52: public lead forms kept only utm_source, utm_medium and utm_campaign; the click
// ids (fbclid, gclid, ttclid) and utm_content/utm_term were lost, so a lead could not be tied to
// its ad. Non-empty values are kept, each capped (the endpoint is public).
func TestAttributionMetadata(t *testing.T) {
	got := attributionMetadata(publicLeadInput{
		FBCLID: "fb.1", GCLID: "", TTCLID: "tt-9", UTMContent: "video-a", UTMTerm: strings.Repeat("x", 400),
	})
	if got["fbclid"] != "fb.1" || got["ttclid"] != "tt-9" || got["utm_content"] != "video-a" {
		t.Fatalf("kept values: %v", got)
	}
	if _, ok := got["gclid"]; ok {
		t.Error("empty values are left out")
	}
	if len(got["utm_term"].(string)) != 256 {
		t.Errorf("values are capped at 256 characters, got %d", len(got["utm_term"].(string)))
	}
}
