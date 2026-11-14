package autopublish

import (
	"slices"
	"testing"

	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
)

// The hold list's restricted keys are the creative restricted categories; the Studio's "Always
// ask me first for" list (marketflow-ui src/lib/studio/hold-categories.ts) uses the same keys.
func TestRestrictedHoldKeysMatchCreative(t *testing.T) {
	if got := creative.RestrictedCategoryKeys(); !slices.Equal(got, custom_agents.RestrictedHoldKeys) {
		t.Fatalf("creative keys %v, custom_agents keys %v", got, custom_agents.RestrictedHoldKeys)
	}
}
