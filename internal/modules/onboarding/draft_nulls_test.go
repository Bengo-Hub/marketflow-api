package onboarding

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
)

// A new business has no brief yet: every list in the draft is a JSON list, never null, because the
// onboarding page reads their length (Flavia FBO, 2026-11-25: "This page couldn't load").
func TestDraftListsAreNeverNull(t *testing.T) {
	d := compose(composeInput{tenant: &ent.Tenant{ID: uuid.New(), Slug: "254200056174", BusinessType: "flp", Metadata: map[string]any{}},
		current: marketingos.BrandContext{}})
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Fatalf("draft has null: %s", b)
	}
}
