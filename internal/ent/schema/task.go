package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Task holds the schema definition for the Task entity.
type Task struct {
	ent.Schema
}

// Fields of the Task.
func (Task) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant"),
		field.String("entity_type").
			Comment("lead | contact | deal"),
		field.UUID("entity_id", uuid.UUID{}).
			Comment("Polymorphic FK"),
		field.String("title").
			NotEmpty().
			MaxLen(255),
		field.String("description").
			Optional().
			Nillable(),
		field.Time("due_at").
			Optional().
			Nillable(),
		field.String("priority").
			Default("normal").
			Comment("low | normal | high | urgent"),
		field.String("status").
			Default("open").
			Comment("open | in_progress | done"),
		field.UUID("assigned_to_user_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("user_id from auth-service"),
		field.Time("completed_at").
			Optional().
			Nillable(),
		field.UUID("created_by", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("user_id or nil for system/agent"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Task.
func (Task) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("tasks").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

// Indexes of the Task.
func (Task) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "entity_type", "entity_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "due_at"),
		index.Fields("tenant_id", "assigned_to_user_id"),
		index.Fields("tenant_id", "status", "due_at"),
	}
}
