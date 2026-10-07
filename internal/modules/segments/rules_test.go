package segments

import (
	"testing"
	"time"
)

func TestContactPredicatesParsesSupportedRules(t *testing.T) {
	now := time.Now()
	rules := map[string]any{
		"match": "all",
		"conditions": []any{
			map[string]any{"field": "lifecycle_stage", "op": "eq", "value": "customer"},
			map[string]any{"field": "contact_type", "op": "in", "value": []any{"customer", "lead"}},
			map[string]any{"field": "subscribed_sms", "op": "eq", "value": true},
			map[string]any{"field": "total_orders", "op": "gte", "value": float64(3)},
			map[string]any{"field": "last_order_at", "op": "within_days", "value": float64(90)},
			map[string]any{"field": "has_phone", "op": "eq", "value": true},
		},
	}
	preds, err := ContactPredicates(rules, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(preds) != 6 {
		t.Fatalf("want 6 predicates (all), got %d", len(preds))
	}
	rules["match"] = "any"
	preds, err = ContactPredicates(rules, now)
	if err != nil || len(preds) != 1 {
		t.Fatalf("any-match folds into one OR predicate: %d %v", len(preds), err)
	}
}

func TestContactPredicatesRejectsUnknownRules(t *testing.T) {
	for _, c := range []map[string]any{
		{"field": "favourite_colour", "op": "eq", "value": "red"},
		{"field": "lifecycle_stage", "op": "like", "value": "cust"},
		{"field": "subscribed_email", "op": "neq", "value": true},
		{"field": "", "op": "eq"},
	} {
		if _, err := ContactPredicates(map[string]any{"conditions": []any{c}}, time.Now()); err == nil {
			t.Errorf("%v must be rejected so a typo never widens a send", c)
		}
	}
}

func TestEmptyRulesMatchEveryone(t *testing.T) {
	preds, err := ContactPredicates(map[string]any{}, time.Now())
	if err != nil || len(preds) != 0 {
		t.Fatalf("got %d %v", len(preds), err)
	}
}
