package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// UserRoleAssignment holds the schema definition for user role assignments.
type UserRoleAssignment struct {
	ent.Schema
}

// Fields of the UserRoleAssignment.
func (UserRoleAssignment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Tenant identifier"),
		field.UUID("user_id", uuid.UUID{}).
			Comment("User identifier (marketflow_user)"),
		field.UUID("role_id", uuid.UUID{}).
			Comment("Role identifier"),
		field.UUID("assigned_by", uuid.UUID{}).
			Comment("User who assigned the role"),
		field.Time("assigned_at").
			Default(time.Now).
			Immutable(),
		field.Time("expires_at").
			Optional().
			Comment("Optional expiration date"),
	}
}

// Edges of the UserRoleAssignment.
func (UserRoleAssignment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("user", MarketflowUser.Type).
			Field("user_id").
			Required().
			Unique(),
		edge.To("role", MarketflowRole.Type).
			Field("role_id").
			Required().
			Unique(),
	}
}

// Indexes of the UserRoleAssignment.
func (UserRoleAssignment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "user_id", "role_id").Unique(),
		index.Fields("tenant_id"),
		index.Fields("user_id"),
		index.Fields("role_id"),
		index.Fields("expires_at"),
	}
}
