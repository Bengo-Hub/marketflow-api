package events

import "testing"

func TestLastNineDigits(t *testing.T) {
	for in, want := range map[string]string{"+254712345678": "712345678", "0712 345 678": "712345678", "12345": "12345"} {
		if got := lastNineDigits(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
