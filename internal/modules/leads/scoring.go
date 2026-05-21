package leads

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/leadscoringrule"
	"github.com/google/uuid"
)

// ScoringCondition is a single condition within a lead scoring rule.
// JSON: {"field": "source", "op": "eq", "value": "meta_ad"}
type ScoringCondition struct {
	Field string `json:"field"`
	Op    string `json:"op"` // eq | neq | contains | gte | lte
	Value any    `json:"value"`
}

// RecalculateScore recomputes the lead score by evaluating all active rules for the tenant.
// The final score is clamped to 0–100.
func (s *Service) RecalculateScore(ctx context.Context, tenantID, leadID uuid.UUID) (*ent.Lead, error) {
	target, err := s.client.Lead.Query().
		Where(lead.ID(leadID), lead.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get lead for scoring: %w", err)
	}

	rules, err := s.client.LeadScoringRule.Query().
		Where(
			leadscoringrule.TenantID(tenantID),
			leadscoringrule.IsActive(true),
		).
		Order(ent.Asc(leadscoringrule.FieldSortOrder)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load scoring rules: %w", err)
	}

	score := 0
	breakdown := make([]map[string]any, 0, len(rules))

	for _, rule := range rules {
		matched := evalConditions(rule.Conditions, target)
		delta := 0
		if matched {
			delta = rule.ScoreDelta
			score += delta
		}
		breakdown = append(breakdown, map[string]any{
			"rule_id":   rule.ID,
			"rule_name": rule.Name,
			"delta":     delta,
			"matched":   matched,
		})
	}

	// Clamp 0–100
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	updated, err := target.Update().
		SetScore(score).
		SetAiScoreBreakdown(map[string]any{"rules": breakdown}).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("save lead score: %w", err)
	}

	s.logger.Sugar().Infow("lead score recalculated",
		"lead_id", leadID,
		"score", score,
		"rules_evaluated", len(rules),
	)
	return updated, nil
}

// evalConditions checks whether all conditions in a rule's JSON map match the given lead.
// Conditions stored as map[string]any with keys matching ScoringCondition fields.
func evalConditions(conditionsMap map[string]any, l *ent.Lead) bool {
	// conditions may be stored as:
	// {"field": "source", "op": "eq", "value": "meta_ad"}   (single condition)
	// {"conditions": [...]}                                    (array wrapper)
	// We support both the flat single-condition form and the "conditions" array form.

	if rawList, ok := conditionsMap["conditions"]; ok {
		// Array form
		list, ok := rawList.([]any)
		if !ok {
			return false
		}
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				return false
			}
			if !evalSingleCondition(m, l) {
				return false
			}
		}
		return true
	}

	// Single condition form
	return evalSingleCondition(conditionsMap, l)
}

func evalSingleCondition(cond map[string]any, l *ent.Lead) bool {
	field, _ := cond["field"].(string)
	op, _ := cond["op"].(string)
	expected := cond["value"]

	var actual any
	switch field {
	case "source":
		actual = l.Source
	case "lead_type":
		actual = l.LeadType
	case "status":
		actual = l.Status
	case "interest":
		if l.Interest != nil {
			actual = *l.Interest
		}
	case "email":
		if l.Email != nil {
			actual = *l.Email
		}
	case "score":
		actual = float64(l.Score)
	default:
		// Unknown field — condition does not match
		return false
	}

	switch op {
	case "eq":
		return fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", expected)
	case "neq":
		return fmt.Sprintf("%v", actual) != fmt.Sprintf("%v", expected)
	case "contains":
		return len(fmt.Sprintf("%v", actual)) > 0 &&
			containsStr(fmt.Sprintf("%v", actual), fmt.Sprintf("%v", expected))
	case "gte":
		av, ae := toFloat(actual), toFloat(expected)
		return av >= ae
	case "lte":
		av, ae := toFloat(actual), toFloat(expected)
		return av <= ae
	}
	return false
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub ||
		(len(sub) > 0 && searchStr(s, sub)))
}

func searchStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}
