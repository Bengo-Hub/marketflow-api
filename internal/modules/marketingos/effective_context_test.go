package marketingos

import (
	"slices"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/platform/shareddefaults"
)

// What marketflow-ai and the lead replies enforce: the shared claims always, plus the business's
// own, with or without a saved brand context (plan item 76).
func TestEffectiveContextBannedClaims(t *testing.T) {
	shared := shareddefaults.SharedClaims("pharmacy")
	for name, meta := range map[string]map[string]any{
		"none saved": nil,
		"own only":   {MetadataKey: map[string]any{"positioning": "p", "banned_claims": []any{"My rule"}}},
		"old copy":   {MetadataKey: map[string]any{"banned_claims": []any{shared[0], "My rule"}}},
	} {
		ten := &ent.Tenant{BusinessType: "pharmacy", Metadata: meta}
		got, _ := EffectiveContext(ten)["banned_claims"].([]string)
		for _, c := range shared {
			if !slices.Contains(got, c) {
				t.Errorf("%s: %q is missing", name, c)
			}
		}
		if own := slices.Contains(got, "My rule"); own != (meta != nil) || len(got) > len(shared)+1 {
			t.Errorf("%s: %v", name, got)
		}
	}
}
