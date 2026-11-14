package config

import (
	"testing"
	"time"
)

// A new Autopilot terms version applies on its effective date, or 14 days after it is announced
// (owner 2026-11-14); with neither set it applies at once.
func TestTermsEffective(t *testing.T) {
	at := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		cfg  AutopilotConfig
		want time.Time
	}{
		{"neither", AutopilotConfig{TermsNotice: 336 * time.Hour}, time.Time{}},
		{"effective date", AutopilotConfig{TermsEffectiveAt: "2026-12-01T00:00:00Z", TermsAnnouncedAt: "2026-11-01T00:00:00Z"}, at},
		{"announced plus notice", AutopilotConfig{TermsAnnouncedAt: "2026-11-17T00:00:00Z", TermsNotice: 336 * time.Hour}, at},
		{"announced, default notice", AutopilotConfig{TermsAnnouncedAt: "2026-11-17T00:00:00Z"}, at},
		{"unparsable", AutopilotConfig{TermsEffectiveAt: "next month"}, time.Time{}},
	}
	for _, c := range cases {
		if got := c.cfg.TermsEffective(); !got.Equal(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
