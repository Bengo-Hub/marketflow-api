package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CustomFieldDef holds the schema definition for the CustomFieldDef entity.
type CustomFieldDef struct {
	ent.Schema
}

// Fields of the CustomFieldDef.
func (CustomFieldDef) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("entity_type").
			Comment("lead | contact | deal | account"),
		field.String("field_key").
			NotEmpty().
			MaxLen(64).
			Comment("snake_case identifier e.g. preferred_product"),
		field.String("field_label").
			NotEmpty().
			MaxLen(128).
			Comment("Display label e.g. Preferred Product"),
		field.String("field_type").
			Default("text").
			Comment("text | number | date | select | multiselect | boolean | url"),
		field.JSON("options", []string{}).
			Optional().
			Comment("For select/multiselect field types"),
		field.Bool("is_required").
			Default(false),
		field.Int("sort_order").
			Default(0),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the CustomFieldDef.
func (CustomFieldDef) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("custom_field_defs").
			Field("tenant_id").
			Required().
			Unique(),
		edge.To("values", CustomFieldValue.Type),
	}
}

// Indexes of the CustomFieldDef.
func (CustomFieldDef) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "entity_type", "field_key").Unique(),
		index.Fields("tenant_id", "entity_type"),
	}
}
