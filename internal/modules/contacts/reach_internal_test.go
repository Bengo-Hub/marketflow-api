package contacts

import (
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
)

func TestConsentRecorded(t *testing.T) {
	cases := map[string]bool{"": false, "legacy_unverified": false, "checkout_opt_in": true, "recorded_by_staff": true}
	for src, want := range cases {
		c := &ent.Contact{Metadata: map[string]any{}}
		if src != "" {
			c.Metadata["consent_source"] = src
		}
		if got := consentRecorded(c); got != want {
			t.Errorf("consent_source %q: got %v, want %v", src, got, want)
		}
	}
}

func TestTurnedOn(t *testing.T) {
	yes, no := true, false
	if !turnedOn(&yes, false) || turnedOn(&yes, true) || turnedOn(&no, false) || turnedOn(nil, false) {
		t.Fatal("only an off-to-on change records consent")
	}
}
