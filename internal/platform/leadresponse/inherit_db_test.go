package leadresponse

import (
	"context"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Plan item 76: a copy inherits the template's steps, its own wording overrides them, and a copy
// made before inheritance (no flag) keeps the steps stored on it.
func TestInheritFollowUpSteps(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tpl, err := DefaultTemplate(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if tpl == nil {
		home := seedTenant(t, db, nil)
		tpl, err = db.NurtureSequence.Create().SetTenantID(home).SetScope("platform").SetName("Instant follow-up").
			SetTriggerEvent(TriggerLeadCreated).SetActive(true).
			SetSteps([]map[string]any{{"delay_hours": 0, "channel": "email", "subject": "Hi {{fbo_name}}", "body": "Hello"}}).
			SetMetadata(map[string]any{TemplateFlag: true}).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	tid := seedTenant(t, db, nil)
	if _, err := ProvisionFollowUp(ctx, db, tid); err != nil {
		t.Fatal(err)
	}
	old := []map[string]any{{"delay_hours": 1, "channel": "email", "subject": "My own words", "body": "x"}}
	read := func(meta map[string]any) *ent.NurtureSequence {
		s := &ent.NurtureSequence{Metadata: meta, Steps: old}
		Inherit(ctx, db, s)
		return s
	}
	if got := read(map[string]any{CopyFlag: true, InheritFlag: true}); !sameSteps(got.Steps, tpl.Steps) {
		t.Fatalf("an inheriting copy has the template's steps: %v", got.Steps)
	}
	for _, meta := range []map[string]any{{CopyFlag: true}, {InheritFlag: true}, nil} {
		if got := read(meta); !sameSteps(got.Steps, old) {
			t.Fatalf("%v must keep its stored steps: %v", meta, got.Steps)
		}
	}

	made, err := db.NurtureSequence.Query().All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range made {
		if s.TenantID == tid {
			found = s.Metadata[InheritFlag] == true
		}
	}
	if !found {
		t.Fatal("a new copy inherits")
	}

	if m := InheritMeta(ctx, db, map[string]any{CopyFlag: true, InheritFlag: true}, old); m[InheritFlag] != nil || m[CopyFlag] != true {
		t.Fatalf("own wording stops inheriting: %v", m)
	}
	same := make([]map[string]any, len(tpl.Steps))
	for i, s := range tpl.Steps {
		same[i] = map[string]any{"a_field_the_form_left_empty": ""}
		for k, v := range s {
			same[i][k] = v
		}
	}
	if m := InheritMeta(ctx, db, map[string]any{CopyFlag: true}, same); m[InheritFlag] != true {
		t.Fatalf("the template's wording inherits again: %v", m)
	}
}
