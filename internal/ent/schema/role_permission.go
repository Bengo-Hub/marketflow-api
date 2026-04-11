package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RolePermission holds the schema definition for the role-permission junction table.
type RolePermission struct {
	ent.Schema
}

// Fields of the RolePermission.
func (RolePermission) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("role_id", uuid.UUID{}).
			Comment("Role identifier"),
		field.UUID("permission_id", uuid.UUID{}).
			Comment("Permission identifier"),
	}
}

// Edges of the RolePermission.
func (RolePermission) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("role", MarketflowRole.Type).
			Field("role_id").
			Required().
			Unique(),
		edge.To("permission", MarketflowPermission.Type).
			Field("permission_id").
			Required().
			Unique(),
	}
}

// Indexes of the RolePermission.
func (RolePermission) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("role_id", "permission_id").Unique(),
		index.Fields("role_id"),
		index.Fields("permission_id"),
	}
}
