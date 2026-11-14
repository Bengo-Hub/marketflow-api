package tenant

import "testing"

// Owner 2026-11-14: the AI label choice is "on" or "off"; null clears it (on).
func TestValidateAILabel(t *testing.T) {
	for _, m := range []map[string]any{nil, {AILabelKey: "on"}, {AILabelKey: "off"}, {AILabelKey: nil}} {
		if err := ValidateAILabel(m); err != nil {
			t.Errorf("%v: unexpected %v", m, err)
		}
	}
	for _, m := range []map[string]any{{AILabelKey: "no"}, {AILabelKey: false}, {AILabelKey: "OFF"}} {
		if err := ValidateAILabel(m); err == nil {
			t.Errorf("%v: want an error", m)
		}
	}
}

// Owner 2026-11-14: only a real change of the AI label needs an owner or admin; a form that sends
// the stored value back does not.
func TestAILabelChanged(t *testing.T) {
	cases := []struct {
		stored map[string]any
		sent   any
		want   bool
	}{
		{nil, "on", false},
		{nil, nil, false},
		{nil, "off", true},
		{map[string]any{AILabelKey: "off"}, "off", false},
		{map[string]any{AILabelKey: "off"}, "on", true},
		{map[string]any{AILabelKey: "off"}, nil, true},
	}
	for _, c := range cases {
		if got := AILabelChanged(c.stored, c.sent); got != c.want {
			t.Errorf("%v -> %v: got %v", c.stored, c.sent, got)
		}
	}
}

// Sprint 23 L45: the sales follow-up settings saved through the profile are checked, so the
// worker never reads a hand-off amount it cannot use.
func TestValidateSalesSettings(t *testing.T) {
	ok := []map[string]any{
		nil,
		{"bio": "x"},
		{SalesHandoffValueKey: 100000.0, SalesAutoReplyKey: true},
		{SalesHandoffValueKey: 0.0},
		{SalesHandoffValueKey: nil, SalesAutoReplyKey: nil}, // null clears
		{SalesAutoReplyKey: false},
	}
	for _, m := range ok {
		if err := ValidateSalesSettings(m); err != nil {
			t.Errorf("%v: unexpected %v", m, err)
		}
	}
	bad := []map[string]any{
		{SalesHandoffValueKey: -1.0},
		{SalesHandoffValueKey: "100k"},
		{SalesHandoffValueKey: 2e12},
		{SalesAutoReplyKey: "yes"},
	}
	for _, m := range bad {
		if err := ValidateSalesSettings(m); err == nil {
			t.Errorf("%v: want an error", m)
		}
	}
}
