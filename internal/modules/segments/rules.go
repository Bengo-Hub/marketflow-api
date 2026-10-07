package segments

import (
	"fmt"
	"strings"
	"time"

	entcontact "github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
)

// Condition is one rule in a segment's filter_rules:
//
//	{"match": "all"|"any", "conditions": [{"field": "lifecycle_stage", "op": "eq", "value": "customer"}]}
//
// Supported contact fields and operators:
//
//	lifecycle_stage, contact_type      eq, neq, in (value: list)
//	subscribed_email, subscribed_sms   eq (value: true/false)
//	total_orders                       eq, gte, lte (value: number)
//	last_order_at, created_at          within_days, older_than_days (value: days)
//	has_email, has_phone               eq (value: true/false)
type Condition struct {
	Field string
	Op    string
	Value any
}

// ContactPredicates turns filter_rules into SQL predicates on contacts. The same predicates count
// a segment and page through its members, so the two never disagree. Empty rules match every
// contact. An unknown field or operator is an error, so a typo never silently widens a send.
func ContactPredicates(rules map[string]any, now time.Time) ([]predicate.Contact, error) {
	conds, err := parseConditions(rules)
	if err != nil {
		return nil, err
	}
	var preds []predicate.Contact
	for _, c := range conds {
		p, err := contactPredicate(c, now)
		if err != nil {
			return nil, err
		}
		preds = append(preds, p)
	}
	if len(preds) == 0 {
		return nil, nil
	}
	if m, _ := rules["match"].(string); strings.EqualFold(m, "any") {
		return []predicate.Contact{entcontact.Or(preds...)}, nil
	}
	return preds, nil
}

func parseConditions(rules map[string]any) ([]Condition, error) {
	raw, ok := rules["conditions"]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("conditions must be a list")
	}
	out := make([]Condition, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("condition %d is not an object", i+1)
		}
		f, _ := m["field"].(string)
		op, _ := m["op"].(string)
		if f == "" || op == "" {
			return nil, fmt.Errorf("condition %d needs a field and an op", i+1)
		}
		out = append(out, Condition{Field: f, Op: op, Value: m["value"]})
	}
	return out, nil
}

func contactPredicate(c Condition, now time.Time) (predicate.Contact, error) {
	bad := func() (predicate.Contact, error) {
		return nil, fmt.Errorf("unsupported condition %s %s", c.Field, c.Op)
	}
	switch c.Field {
	case "lifecycle_stage", "contact_type":
		eq, in := entcontact.LifecycleStageEQ, entcontact.LifecycleStageIn
		neq := entcontact.LifecycleStageNEQ
		if c.Field == "contact_type" {
			eq, in, neq = entcontact.ContactTypeEQ, entcontact.ContactTypeIn, entcontact.ContactTypeNEQ
		}
		switch c.Op {
		case "eq":
			return eq(str(c.Value)), nil
		case "neq":
			return neq(str(c.Value)), nil
		case "in":
			return in(strs(c.Value)...), nil
		}
	case "subscribed_email", "subscribed_sms":
		if c.Op != "eq" {
			return bad()
		}
		if c.Field == "subscribed_email" {
			return entcontact.SubscribedEmail(truthy(c.Value)), nil
		}
		return entcontact.SubscribedSms(truthy(c.Value)), nil
	case "has_email", "has_phone":
		if c.Op != "eq" {
			return bad()
		}
		has := entcontact.And(entcontact.EmailNotNil(), entcontact.EmailNEQ(""))
		if c.Field == "has_phone" {
			has = entcontact.And(entcontact.PhoneNotNil(), entcontact.PhoneNEQ(""))
		}
		if truthy(c.Value) {
			return has, nil
		}
		return entcontact.Not(has), nil
	case "total_orders":
		n := int(num(c.Value))
		switch c.Op {
		case "eq":
			return entcontact.TotalOrdersEQ(n), nil
		case "gte":
			return entcontact.TotalOrdersGTE(n), nil
		case "lte":
			return entcontact.TotalOrdersLTE(n), nil
		}
	case "last_order_at", "created_at":
		days := num(c.Value)
		if days < 0 {
			return bad()
		}
		cutoff := now.Add(-time.Duration(days * float64(24*time.Hour)))
		switch c.Op {
		case "within_days":
			if c.Field == "created_at" {
				return entcontact.CreatedAtGTE(cutoff), nil
			}
			return entcontact.LastOrderAtGTE(cutoff), nil
		case "older_than_days":
			if c.Field == "created_at" {
				return entcontact.CreatedAtLT(cutoff), nil
			}
			return entcontact.LastOrderAtLT(cutoff), nil
		}
	}
	return bad()
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{l}
	}
	return nil
}

func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true") || b == "1"
	case float64:
		return b != 0
	}
	return false
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
