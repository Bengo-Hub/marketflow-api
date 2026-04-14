package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// NurtureEnrollment represents a single lead's walk through one NurtureSequence.
//
// The step scheduler (marketflow-worker) queries WHERE next_step_at <= now AND
// status='active' every 60s and publishes `nurture.step.due` for each match.
// After a successful dispatch the handler advances current_step and recomputes
// next_step_at from the sequence's steps[] definition.
type NurtureEnrollment struct {
	ent.Schema
}

// Fields of the NurtureEnrollment.
func (NurtureEnrollment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("sequence_id", uuid.UUID{}),
		field.UUID("lead_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("contact_id", uuid.UUID{}).
			Optional().
			Nillable(),

		field.Int("current_step").
			Default(0).
			Comment("Zero-based index into the sequence's steps[] array"),
		field.String("status").
			Default("active").
			Comment("active | completed | paused | cancelled | errored"),

		field.Time("next_step_at").
			Optional().
			Nillable().
			Comment("When the scheduler should dispatch the next step"),
		field.Time("last_step_at").
			Optional().
			Nillable(),
		field.Time("started_at").
			Default(time.Now).
			Immutable(),
		field.Time("completed_at").
			Optional().
			Nillable(),

		field.String("exit_reason").
			Optional().
			Nillable().
			Comment("completed | unsubscribed | converted_early | errored | cancelled_by_admin"),

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

// Edges of the NurtureEnrollment.
func (NurtureEnrollment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("nurture_enrollments").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

// Indexes optimise the scheduler's primary query and per-lead lookup.
func (NurtureEnrollment) Indexes() []ent.Index {
	return []ent.Index{
		// Scheduler: "what's due right now?"
		index.Fields("status", "next_step_at"),
		// Per-tenant listings + dedup ("is this lead already enrolled?")
		index.Fields("tenant_id", "lead_id", "sequence_id"),
		index.Fields("tenant_id", "status"),
	}
}
