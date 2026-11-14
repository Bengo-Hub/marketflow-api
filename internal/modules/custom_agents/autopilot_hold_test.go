package custom_agents

import (
	"slices"
	"testing"
)

// The "Always ask me first for" list (owner 2026-11-14): restricted categories match on the
// offering's own text through the matcher, the tenant's words match as whole words.
func TestHeldFor(t *testing.T) {
	restricted := func(texts ...string) []string {
		for _, s := range texts {
			if s == "Panadol tablets" {
				return []string{"health"}
			}
		}
		return nil
	}
	cases := []struct {
		name     string
		hold     []string
		offering map[string]any
		want     string
	}{
		{"exact category, as before", []string{"services"}, map[string]any{"type": "Services"}, "services"},
		{"restricted key touched by the name", []string{"health"}, map[string]any{"name": "Panadol tablets"}, "health"},
		{"restricted key not kept", []string{"alcohol"}, map[string]any{"name": "Panadol tablets"}, ""},
		{"restricted key not touched", []string{"health"}, map[string]any{"name": "Leather shoes"}, ""},
		{"tenant word in the description", []string{"wedding cake"}, map[string]any{"name": "Cake", "description": "Our Wedding Cake, three tiers"}, "wedding cake"},
		{"word inside a longer word does not match", []string{"gin"}, map[string]any{"name": "Original ginger beer"}, ""},
		{"word in the title", []string{"gin"}, map[string]any{"title": "Gin & tonic night"}, "gin"},
		{"nothing kept", nil, map[string]any{"name": "Panadol tablets"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HeldFor(c.hold, c.offering, restricted); got != c.want {
				t.Fatalf("HeldFor = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRestrictedHoldKeysSorted(t *testing.T) {
	if !slices.IsSorted(RestrictedHoldKeys) || len(RestrictedHoldKeys) != 8 {
		t.Fatalf("RestrictedHoldKeys = %v", RestrictedHoldKeys)
	}
}
