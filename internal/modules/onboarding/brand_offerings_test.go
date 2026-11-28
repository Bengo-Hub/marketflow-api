package onboarding

import "testing"

// A business without inventory advertises its brand context offers (owner, 2026-11-28); the line's
// name and any KES price are read from the owner's own wording.
func TestSplitOfferLine(t *testing.T) {
	cases := []struct {
		line, name string
		price      float64
	}{
		{"Shea Butter Body Lotion 250ml (KES 850)", "Shea Butter Body Lotion 250ml", 850},
		{"Gift registry: free to create", "Gift registry", 0},
		{"Wedding cash fund (Ksh 1,200.50)", "Wedding cash fund", 1200.50},
		{"M-Pesa contributions", "M-Pesa contributions", 0},
	}
	for _, c := range cases {
		name, price := splitOfferLine(c.line)
		if name != c.name {
			t.Errorf("%q: name %q, want %q", c.line, name, c.name)
		}
		switch {
		case c.price == 0 && price != nil:
			t.Errorf("%q: price %v, want none", c.line, *price)
		case c.price != 0 && (price == nil || *price != c.price):
			t.Errorf("%q: price %v, want %v", c.line, price, c.price)
		}
	}
}
