package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PipelineStage holds the schema definition for the PipelineStage entity.
// A stage is a single step within a Pipeline through which deals progress.
type PipelineStage struct {
	ent.Schema
}

// Fields of the PipelineStage.
func (PipelineStage) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Denormalized for indexed queries"),
		field.UUID("pipeline_id", uuid.UUID{}),

		field.String("name").
			NotEmpty(),
		field.Int("stage_order").
			Default(0).
			Comment("Sort order within pipeline"),
		field.Float("win_probability").
			Default(0.0).
			Comment("0.0-1.0 weighted revenue probability"),
		field.Bool("is_won_stage").
			Default(false),
		field.Bool("is_lost_stage").
			Default(false),
		field.String("color").
			Optional().
			Nillable().
			Comment("Hex color for Kanban card e.g. #22c55e"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the PipelineStage.
func (PipelineStage) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("pipeline", Pipeline.Type).
			Ref("stages").
			Field("pipeline_id").
			Required().
			Unique(),
		edge.To("deals", Deal.Type),
	}
}

// Indexes of the PipelineStage.
func (PipelineStage) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("pipeline_id", "stage_order"),
		index.Fields("tenant_id"),
	}
}
