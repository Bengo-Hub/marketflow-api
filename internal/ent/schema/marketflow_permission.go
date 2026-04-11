package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// MarketflowPermission holds the schema definition for service-level permissions.
// Permission codes follow format: marketflow.{module}.{action}
type MarketflowPermission struct {
	ent.Schema
}

// Fields of the MarketflowPermission.
func (MarketflowPermission) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("permission_code").
			NotEmpty().
			Unique().
			Comment("Permission code: marketflow.leads.add, marketflow.leads.view, etc."),
		field.String("name").
			NotEmpty().
			Comment("Display name"),
		field.String("module").
			NotEmpty().
			Comment("Module: leads, contacts, knowledge, campaigns, funnels, shortlinks, providers, config, analytics, chat"),
		field.String("action").
			NotEmpty().
			Comment("Action: add, view, view_own, change, change_own, delete, manage"),
		field.String("resource").
			Optional().
			Comment("Optional resource qualifier"),
		field.Text("description").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the MarketflowPermission.
func (MarketflowPermission) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("roles", MarketflowRole.Type).Ref("permissions").Through("role_permissions", RolePermission.Type),
	}
}

// Indexes of the MarketflowPermission.
func (MarketflowPermission) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("permission_code").Unique(),
		index.Fields("module"),
		index.Fields("action"),
		index.Fields("module", "action"),
	}
}
