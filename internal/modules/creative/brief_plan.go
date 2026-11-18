package creative

import "encoding/json"

// The Creative Plan the owner chose in the Studio form (Sprint 15), kept as the brief's
// metadata.plan: marketflow-ai's quote input, the chosen option (its label, counts and credits)
// and the quote's context. It records what the owner planned. Every render is still priced and
// charged by marketflow-ai when it is made, so a plan a client sends cannot lower a price; the
// checks here only keep the record well formed and small.

// maxBriefPlanBytes caps metadata.plan; a whole quote with its alternatives, lines and notes is a
// few KB.
const maxBriefPlanBytes = 16 << 10

// validateBriefPlan checks metadata.plan when a brief carries one, at most maxBriefPlanBytes as
// JSON. Two shapes are kept there: the Studio form's choice (an option object) and the Marketing
// Manager's whole quote, which marketflow-ai's make-media writes (a recommended object, the
// registry.CreativePlan). Either one's credits must be a whole number of 0 or more.
func validateBriefPlan(meta map[string]any) error {
	raw, ok := meta["plan"]
	if !ok || raw == nil {
		return nil
	}
	plan, ok := raw.(map[string]any)
	if !ok {
		return invalid("metadata.plan must be an object")
	}
	if b, err := json.Marshal(plan); err != nil || len(b) > maxBriefPlanBytes {
		return invalid("metadata.plan is too large")
	}
	option, ok := plan["option"].(map[string]any)
	if !ok {
		// The Marketing Manager's quote: its recommended option.
		if option, ok = plan["recommended"].(map[string]any); !ok {
			return invalid("metadata.plan needs the chosen option (or the quote's recommended one)")
		}
	}
	credits, ok := option["credits"].(float64)
	if !ok || credits < 0 || credits != float64(int64(credits)) {
		return invalid("metadata.plan's option credits must be a whole number of 0 or more")
	}
	return nil
}
