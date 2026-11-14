package tenant

import "errors"

// Sprint 23 L45: sales follow-up settings, kept in Tenant.metadata and saved through the
// profile PATCH. marketflow-worker reads them when a lead replies on WhatsApp.
const (
	// SalesHandoffValueKey is the budget at or above which a lead goes to the owner (0 or
	// unset: amounts never hand off; asking for a person always does).
	SalesHandoffValueKey = "sales_handoff_value"
	// SalesAutoReplyKey switches the assistant's answers to lead replies (unset means on).
	SalesAutoReplyKey = "sales_auto_reply"
)

// AILabelKey is the owner's AI label choice, "on" (default) or "off" (owner 2026-11-14), saved
// through the profile PATCH. It is creative.AILabelKey; marketflow-ai and the autopilot read it
// through creative.TenantAILabel.
const AILabelKey = "ai_label"

// ValidateAILabel checks the AI label key of a profile metadata update. A null value clears it,
// which means on.
func ValidateAILabel(meta map[string]any) error {
	if v, ok := meta[AILabelKey]; ok && v != nil {
		if s, isStr := v.(string); !isStr || (s != "on" && s != "off") {
			return errors.New(`ai_label must be "on" or "off"`)
		}
	}
	return nil
}

// AILabelChanged reports whether sending v would change the stored AI label. Unset, null and
// "on" are all on.
func AILabelChanged(stored map[string]any, v any) bool {
	return aiLabelOff(stored[AILabelKey]) != aiLabelOff(v)
}

func aiLabelOff(x any) bool {
	s, _ := x.(string)
	return s == "off"
}

const maxSalesHandoffValue = 1e11

// ValidateSalesSettings checks the sales keys of a profile metadata update. A null value
// clears the key.
func ValidateSalesSettings(meta map[string]any) error {
	if v, ok := meta[SalesHandoffValueKey]; ok && v != nil {
		n, isNum := v.(float64)
		if !isNum || n < 0 || n > maxSalesHandoffValue {
			return errors.New("sales_handoff_value must be a number from 0 up to 100 billion")
		}
	}
	if v, ok := meta[SalesAutoReplyKey]; ok && v != nil {
		if _, isBool := v.(bool); !isBool {
			return errors.New("sales_auto_reply must be true or false")
		}
	}
	return nil
}
