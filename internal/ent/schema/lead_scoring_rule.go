package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// LeadScoringRule holds the schema definition for the LeadScoringRule entity.
type LeadScoringRule struct {
	ent.Schema
}

// Fields of the LeadScoringRule.
func (LeadScoringRule) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").
			NotEmpty().
			MaxLen(128),
		field.JSON("conditions", map[string]any{}).
			Comment("Array of {field, op, value} conditions (AND logic)"),
		field.Int("score_delta").
			Comment("Points to add (positive) or subtract (negative)"),
		field.Bool("is_active").
			Default(true),
		field.Int("sort_order").
			Default(0).
			Comment("Applied in this order"),
		field.String("apply_mode").
			Default("cumulative").
			Comment("first_match | cumulative"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the LeadScoringRule.
func (LeadScoringRule) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("lead_scoring_rules").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

// Indexes of the LeadScoringRule.
func (LeadScoringRule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "is_active"),
		index.Fields("tenant_id", "sort_order"),
	}
}
