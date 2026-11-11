package tenant

import "testing"

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
