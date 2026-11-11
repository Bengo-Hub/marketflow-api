package search

import (
	"reflect"
	"strings"
	"testing"
)

func TestTerms(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"   ", []string{}},
		{"test", []string{"test"}},
		{"Test Customer", []string{"Test", "Customer"}},
		{"  test   customer  ", []string{"test", "customer"}},
		{"test Test TEST", []string{"test"}},
		{"a b c d e f g", []string{"a", "b", "c", "d", "e"}},
	}
	for _, c := range cases {
		if got := Terms(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Terms(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("x", MaxTermLen+10)
	if got := Terms(long); len(got) != 1 || len(got[0]) != MaxTermLen {
		t.Errorf("long term not capped: %d", len(got[0]))
	}
}
