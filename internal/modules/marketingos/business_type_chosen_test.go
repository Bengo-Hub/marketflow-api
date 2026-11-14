package marketingos

import (
	"encoding/json"
	"strings"
	"testing"
)

// An explicit business type choice is final (owner, 2026-11-14); marketflow-ai reads the flag
// from the OS context.
func TestBusinessTypeChosen(t *testing.T) {
	if BusinessTypeChosen(nil) || BusinessTypeChosen(map[string]any{}) || BusinessTypeChosen(map[string]any{BusinessTypeChosenKey: ""}) {
		t.Fatal("no timestamp means never chosen")
	}
	if !BusinessTypeChosen(map[string]any{BusinessTypeChosenKey: "2026-11-14T10:00:00Z"}) {
		t.Fatal("a timestamp means chosen")
	}
	b, _ := json.Marshal(OSContext{BusinessType: "generic", BusinessTypeChosen: true})
	if !strings.Contains(string(b), `"business_type_chosen":true`) {
		t.Fatalf("the flag must reach marketflow-ai: %s", b)
	}
}
