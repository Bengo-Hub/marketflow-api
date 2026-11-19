package googleads

import (
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Only known events map, and only to this account's own conversion actions.
func TestCleanMapping(t *testing.T) {
	got, bad := CleanMapping("123-456-7890", map[string]string{"Lead": " customers/1234567890/conversionActions/9 ", "Purchase": ""})
	if bad != "" || got["Lead"] != "customers/1234567890/conversionActions/9" || got["Purchase"] != nil {
		t.Fatalf("clean: %v %q", got, bad)
	}
	if _, bad := CleanMapping("1234567890", map[string]string{"Lead": "customers/999/conversionActions/9"}); bad == "" {
		t.Fatal("another account's action must be refused")
	}
	if _, bad := CleanMapping("1234567890", map[string]string{"Signup": "customers/1234567890/conversionActions/9"}); bad == "" {
		t.Fatal("unknown events must be refused")
	}
}

func TestMappingOf(t *testing.T) {
	a := &ent.ConnectedAccount{Metadata: map[string]any{MetaConversionActions: map[string]any{"Lead": "customers/1/conversionActions/2", "x": 3}}}
	m := MappingOf(a)
	if len(m) != 1 || m["Lead"] != "customers/1/conversionActions/2" {
		t.Fatalf("mapping: %v", m)
	}
}
