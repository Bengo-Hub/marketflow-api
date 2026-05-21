package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Segment holds the schema definition for the Segment entity.
type Segment struct {
	ent.Schema
}

// Fields of the Segment.
func (Segment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").
			NotEmpty().
			MaxLen(128),
		field.String("entity_type").
			Default("contact").
			Comment("lead | contact"),
		field.JSON("filter_rules", map[string]any{}).
			Comment("Array of {field, op, value, logic} filter rules"),
		field.Bool("is_dynamic").
			Default(true).
			Comment("Recalculated daily if true"),
		field.Int("member_count").
			Default(0).
			Comment("Cached member count"),
		field.Time("last_calculated_at").
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

// Edges of the Segment.
func (Segment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("segments").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

// Indexes of the Segment.
func (Segment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "entity_type"),
		index.Fields("tenant_id", "is_dynamic"),
	}
}
