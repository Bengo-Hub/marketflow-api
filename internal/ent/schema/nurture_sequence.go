package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// NurtureSequence holds the schema definition for the NurtureSequence entity.
// An automated email/notification sequence triggered by lead lifecycle events.
type NurtureSequence struct {
	ent.Schema
}

// Fields of the NurtureSequence.
func (NurtureSequence) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant"),

		field.String("name").
			NotEmpty().
			Comment("Sequence display name"),
		field.String("trigger_event").
			NotEmpty().
			Comment("Event that starts this sequence: lead.created | lead.qualified | contact.dormant"),
		field.String("lead_type_filter").
			Optional().
			Nillable().
			Comment("Only trigger for leads of this type: customer | recruit"),
		field.Bool("active").
			Default(true),

		// Sequence steps
		field.JSON("steps", []map[string]any{}).
			Comment("Ordered array of {delay_hours, action, template_id, subject, body}"),

		// Stats
		field.Int("total_enrolled").
			Default(0).
			Comment("Total leads/contacts that entered this sequence"),
		field.Int("total_completed").
			Default(0).
			Comment("Total that completed all steps"),

		field.JSON("metadata", map[string]any{}).
			Optional(),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the NurtureSequence.
func (NurtureSequence) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("nurture_sequences").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

// Indexes of the NurtureSequence.
func (NurtureSequence) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "trigger_event"),
		index.Fields("tenant_id", "active"),
	}
}
