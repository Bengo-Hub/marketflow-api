package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CustomFieldValue holds the schema definition for the CustomFieldValue entity.
type CustomFieldValue struct {
	ent.Schema
}

// Fields of the CustomFieldValue.
func (CustomFieldValue) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("entity_type").
			Comment("lead | contact | deal | account"),
		field.UUID("entity_id", uuid.UUID{}).
			Comment("Polymorphic FK"),
		field.UUID("field_def_id", uuid.UUID{}),
		field.String("value").
			Optional().
			Nillable().
			Comment("Serialized string; parse by field_def.field_type"),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the CustomFieldValue.
func (CustomFieldValue) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("custom_field_values").
			Field("tenant_id").
			Required().
			Unique(),
		edge.From("field_def", CustomFieldDef.Type).
			Ref("values").
			Field("field_def_id").
			Required().
			Unique(),
	}
}

// Indexes of the CustomFieldValue.
func (CustomFieldValue) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("entity_type", "entity_id", "field_def_id").Unique(),
		index.Fields("tenant_id", "entity_type", "entity_id"),
		index.Fields("field_def_id"),
	}
}
