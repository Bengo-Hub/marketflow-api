package onboarding

import "testing"

func TestProductLine(t *testing.T) {
	p := WebsiteProduct{Name: "POS System", Description: "Offline-first point of sale.", URL: "https://pos.codevertexafrica.com",
		Highlights: []string{"Works offline", "KRA eTIMS ready"}}
	want := "Offline-first point of sale. Works offline, KRA eTIMS ready. Get it at https://pos.codevertexafrica.com."
	if got := productLine(p); got != want {
		t.Fatalf("got %q", got)
	}
	if got := productLine(WebsiteProduct{Tagline: "Books for SMEs", URL: "/books"}); got != "Books for SMEs." {
		t.Fatalf("relative link dropped, got %q", got)
	}
	if productLine(WebsiteProduct{}) != "" {
		t.Fatal("empty product")
	}
}
