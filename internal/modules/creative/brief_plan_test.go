package creative

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// metadata.plan is optional; when present it must be an object with an option whose credits is a
// whole number of 0 or more, and small.
func TestValidateBriefPlan(t *testing.T) {
	parse := func(s string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	ok := []string{
		`{}`,
		`{"brand":{"accent":"#EC4899"}}`,
		`{"plan":null}`,
		`{"plan":{"option":{"label":"Test 4, make 2","credits":8},"recommended":true,"input":{"ad_budget_kes":4900,"days":7}}}`,
		`{"plan":{"option":{"credits":0}}}`,
		// The Marketing Manager's whole quote, as marketflow-ai's make-media writes it.
		`{"plan":{"recommended":{"label":"Test 4, make 2","credits":12,"lines":[]},"alternatives":[{"credits":6}],"max_live_ads":2,"credits_available":40}}`,
	}
	for _, s := range ok {
		if err := validateBriefPlan(parse(s)); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	bad := []string{
		`{"plan":"cheap"}`,
		`{"plan":{}}`,
		`{"plan":{"option":"best"}}`,
		`{"plan":{"option":{}}}`,
		`{"plan":{"option":{"credits":-1}}}`,
		`{"plan":{"option":{"credits":2.5}}}`,
		`{"plan":{"option":{"credits":"8"}}}`,
		`{"plan":{"recommended":{"credits":-1}}}`,
		`{"plan":{"recommended":"best"}}`,
		`{"plan":{"option":{"credits":8},"notes":["` + strings.Repeat("x", maxBriefPlanBytes) + `"]}}`,
	}
	for _, s := range bad {
		if err := validateBriefPlan(parse(s)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%.80s: want ErrInvalid, got %v", s, err)
		}
	}
	if err := validateBriefPlan(nil); err != nil {
		t.Errorf("no metadata: %v", err)
	}
}

// The check runs as part of a brief's validation, on create and on update.
func TestBriefInputChecksPlan(t *testing.T) {
	title := "Blender ad"
	in := BriefInput{Title: &title, Metadata: map[string]any{"plan": map[string]any{"option": map[string]any{"credits": -3.0}}}}
	if err := in.validate(true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("create with a bad plan: %v", err)
	}
	in.Metadata["plan"] = map[string]any{"option": map[string]any{"credits": 3.0}}
	if err := in.validate(true); err != nil {
		t.Fatalf("create with a good plan: %v", err)
	}
	if err := (&BriefInput{Metadata: map[string]any{"plan": "x"}}).validate(false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("update with a bad plan: %v", err)
	}
}
