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
