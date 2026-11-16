package tenant

import (
	"strings"
	"testing"
)

func TestValidateTenantName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain", "Mama Mboga Fresh", false},
		{"accents and ampersand", "Café & Co. Nairobi", false},
		{"empty is left to the caller", "", false},
		{"exactly 120 runes", strings.Repeat("é", 120), false},
		{"121 runes", strings.Repeat("a", 121), true},
		{"spaces around do not count", "  " + strings.Repeat("a", 120) + "  ", false},
		{"script tag", "<script>alert(1)</script>", true},
		{"greater than", "Best > Rest", true},
		{"newline", "Shop\nName", true},
		{"tab", "Shop\tName", true},
		{"nul", "Shop\x00", true},
		{"delete char", "Shop\x7f", true},
		{"invalid utf8", "Shop\xff", true},
	}
	for _, c := range cases {
		if err := ValidateTenantName(c.in); (err != nil) != c.wantErr {
			t.Errorf("%s: err %v", c.name, err)
		}
	}
}
