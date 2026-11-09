package marketingos

import (
	"strings"
	"testing"
)

func TestMarketQuery(t *testing.T) {
	cases := map[string]string{
		"Coding classes for kids in Kisumu. Small groups, real projects.": "Coding classes for kids in Kisumu",
		"  Bridal makeup in Nairobi!  We travel.":                          "Bridal makeup in Nairobi",
		"":                                                                  "",
	}
	for in, want := range cases {
		if got := marketQuery(in); got != want {
			t.Errorf("marketQuery(%q) = %q, want %q", in, got, want)
		}
	}
	long := marketQuery(strings.Repeat("word ", 60))
	if len([]rune(long)) > 120 || strings.HasSuffix(long, " ") {
		t.Errorf("long query = %q", long)
	}
}

func TestReadMarketSuggestions(t *testing.T) {
	if readMarketSuggestions(map[string]any{}) != nil {
		t.Fatal("no key should read as nil")
	}
	got := readMarketSuggestions(map[string]any{MarketSuggestionsKey: map[string]any{
		"query": "bakery Kisumu", "status": "ready", "themes": []any{map[string]any{"key": "slow_service"}},
	}})
	if got == nil || got.Status != "ready" || !strings.Contains(string(got.Themes), "slow_service") {
		t.Fatalf("read = %+v", got)
	}
}
