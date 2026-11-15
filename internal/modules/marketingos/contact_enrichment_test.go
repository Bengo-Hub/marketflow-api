package marketingos

import "testing"

func TestProfileClear(t *testing.T) {
	on := Settings{ContactEnrichment: true}
	onWeb := Settings{ContactEnrichment: true, ContactEnrichmentWeb: true}
	off := Settings{}
	cases := []struct {
		name   string
		stored map[string]any
		in     Settings
		want   any
		set    bool
	}{
		{"switching off clears every profile", map[string]any{}, off, ClearAllProfiles, true},
		{"already off keeps the pending clear", map[string]any{ContactEnrichmentKey: false, ContactEnrichmentClearKey: ClearAllProfiles}, off, nil, false},
		{"website off clears the descriptions", map[string]any{ContactEnrichmentWebKey: true}, on, ClearCompanySummaries, true},
		{"website off never downgrades a full clear", map[string]any{ContactEnrichmentWebKey: true, ContactEnrichmentClearKey: ClearAllProfiles}, on, nil, true},
		{"back on cancels the full clear", map[string]any{ContactEnrichmentKey: false, ContactEnrichmentClearKey: ClearAllProfiles}, on, nil, true},
		{"website back on cancels its clear", map[string]any{ContactEnrichmentClearKey: ClearCompanySummaries}, onWeb, nil, true},
		{"no change writes nothing", map[string]any{}, on, nil, false},
	}
	for _, c := range cases {
		got, set := profileClear(c.stored, c.in)
		if got != c.want || set != c.set {
			t.Errorf("%s: got %v %v, want %v %v", c.name, got, set, c.want, c.set)
		}
	}
}
