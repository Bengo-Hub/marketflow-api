package leadresponse

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// InheritFlag in a default copy's metadata: the copy uses the platform template's current steps,
// whose variables carry the business's own name and links, instead of the steps stored on it
// (plan item 76). Saving different steps takes the flag off, so a business's own wording stays its
// own; saving the template's wording puts it back. The stored steps are the fallback while there
// is no template. Copies made before this have no flag and keep their stored steps.
const InheritFlag = "inherit"

// Inherit replaces the steps of inheriting default copies with the platform template's, in memory.
func Inherit(ctx context.Context, db *ent.Client, seqs ...*ent.NurtureSequence) {
	var tpl *ent.NurtureSequence
	for _, s := range seqs {
		if !IsDefaultCopy(s) || s.Metadata[InheritFlag] != true {
			continue
		}
		if tpl == nil {
			var err error
			if tpl, err = DefaultTemplate(ctx, db); err != nil || tpl == nil {
				return
			}
		}
		s.Steps = tpl.Steps
	}
}

// InheritMeta is the metadata a default copy has once steps are saved on it.
func InheritMeta(ctx context.Context, db *ent.Client, meta map[string]any, steps []map[string]any) map[string]any {
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	delete(out, InheritFlag)
	if tpl, err := DefaultTemplate(ctx, db); err == nil && tpl != nil && sameSteps(tpl.Steps, steps) {
		out[InheritFlag] = true
	}
	return out
}

// sameSteps compares steps, ignoring empty fields a form leaves in or out.
func sameSteps(a, b []map[string]any) bool {
	x, errX := json.Marshal(filled(a))
	y, errY := json.Marshal(filled(b))
	return errX == nil && errY == nil && bytes.Equal(x, y)
}

func filled(steps []map[string]any) []map[string]any {
	out := make([]map[string]any, len(steps))
	for i, s := range steps {
		out[i] = map[string]any{}
		for k, v := range s {
			if v != nil && v != "" && v != false {
				out[i][k] = v
			}
		}
	}
	return out
}
