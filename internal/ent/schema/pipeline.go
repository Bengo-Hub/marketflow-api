package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Pipeline holds the schema definition for the Pipeline entity.
// A pipeline is a configurable sales funnel composed of ordered stages.
type Pipeline struct {
	ent.Schema
}

// Fields of the Pipeline.
func (Pipeline) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant"),

		field.String("name").
			NotEmpty(),
		field.Bool("is_default").
			Default(false).
			Comment("Only one default per tenant"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Pipeline.
func (Pipeline) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("pipelines").
			Field("tenant_id").
			Required().
			Unique(),
		edge.To("stages", PipelineStage.Type),
		edge.To("deals", Deal.Type),
	}
}

// Indexes of the Pipeline.
func (Pipeline) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("tenant_id", "is_default"),
	}
}
