package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AgentRun holds the schema definition for the AgentRun entity.
type AgentRun struct {
	ent.Schema
}

// Fields of the AgentRun.
func (AgentRun) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("custom_agent_id", uuid.UUID{}),
		field.String("triggered_by").
			Comment("NATS event subject, manual, or cron"),
		field.JSON("trigger_payload", map[string]any{}).
			Optional().
			Comment("Event payload that triggered the run"),
		field.String("status").
			Default("running").
			Comment("running | completed | failed"),
		field.JSON("trace", map[string]any{}).
			Optional().
			Comment("Loop.Trace output: iterations, tool calls, tokens"),
		field.Int("total_tokens").
			Default(0),
		field.Int("tool_calls_n").
			Default(0),
		field.Time("started_at").
			Default(time.Now).
			Immutable(),
		field.Time("finished_at").
			Optional().
			Nillable(),
		field.String("error_message").
			Optional().
			Nillable().
			Comment("Error message if status=failed"),
	}
}

// Edges of the AgentRun.
func (AgentRun) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("agent_runs").
			Field("tenant_id").
			Required().
			Unique(),
		edge.From("custom_agent", CustomAgent.Type).
			Ref("runs").
			Field("custom_agent_id").
			Required().
			Unique(),
	}
}

// Indexes of the AgentRun.
func (AgentRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "custom_agent_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("started_at"),
	}
}
