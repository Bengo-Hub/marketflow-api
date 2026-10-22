package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CustomAgent holds the schema definition for the CustomAgent entity.
type CustomAgent struct {
	ent.Schema
}

// Fields of the CustomAgent.
func (CustomAgent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").
			NotEmpty().
			MaxLen(128),
		field.String("description").
			Optional().
			Nillable(),
		field.String("trigger_type").
			Default("manual").
			Comment("event | cron | manual"),
		field.JSON("trigger_config", map[string]any{}).
			Optional().
			Comment("{event: subject} or {cron: expression}"),
		field.String("system_prompt").
			Optional().
			Nillable().
			Comment("Vera's instruction for this agent"),
		field.JSON("allowed_tools", []string{}).
			Optional().
			Comment("Subset of available tool names"),
		field.Int("max_iterations").
			Default(5).
			Comment("Max agent loop iterations; max 10 (Pro: 20)"),
		field.Bool("is_active").
			Default(true),
		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("kind (crm | team_member | ad_autopilot | content_planner), role, reports_to, skills, model_class, definition, autopilot, template, template_ref, versions"),
		field.Int("run_count").
			Default(0),
		field.Time("last_run_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the CustomAgent.
func (CustomAgent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("custom_agents").
			Field("tenant_id").
			Required().
			Unique(),
		edge.To("runs", AgentRun.Type),
	}
}

// Indexes of the CustomAgent.
func (CustomAgent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "is_active"),
		index.Fields("tenant_id", "trigger_type"),
	}
}
