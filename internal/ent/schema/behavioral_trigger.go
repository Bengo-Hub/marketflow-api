package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// BehavioralTrigger holds the schema definition for the BehavioralTrigger entity.
type BehavioralTrigger struct {
	ent.Schema
}

// Fields of the BehavioralTrigger.
func (BehavioralTrigger) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").
			NotEmpty().
			MaxLen(128),
		field.String("event_type").
			Comment("shortlink.clicked | funnel.step_completed | email.opened | lead.score_changed | contact.created"),
		field.JSON("conditions", map[string]any{}).
			Optional().
			Comment("Array of {field, op, value} conditions"),
		field.JSON("action", map[string]any{}).
			Comment("{type: enroll_sequence|create_task|update_field, ...params}"),
		field.Bool("is_active").
			Default(true),
		field.Int("fire_count").
			Default(0).
			Comment("Total times this trigger has fired"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the BehavioralTrigger.
func (BehavioralTrigger) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("behavioral_triggers").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

// Indexes of the BehavioralTrigger.
func (BehavioralTrigger) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "event_type", "is_active"),
		index.Fields("tenant_id", "is_active"),
	}
}
