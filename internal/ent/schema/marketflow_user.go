package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// MarketflowUser holds the schema definition for JIT-provisioned local user references.
// Links to auth-service users. Created on first request via EnsureUserFromToken.
type MarketflowUser struct {
	ent.Schema
}

// Fields of the MarketflowUser.
func (MarketflowUser) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Tenant identifier"),
		field.UUID("auth_service_user_id", uuid.UUID{}).
			Unique().
			Comment("Reference to auth-service user (no duplication)"),
		field.String("email").
			NotEmpty().
			Comment("Denormalized email for convenience"),
		field.String("status").
			Default("active").
			Comment("Status: active, inactive, suspended"),
		field.String("sync_status").
			Default("synced").
			Comment("Sync status: synced, pending, failed"),
		field.Time("last_sync_at").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the MarketflowUser.
func (MarketflowUser) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("auth_service_user_id").Unique(),
		index.Fields("tenant_id", "auth_service_user_id").Unique(),
		index.Fields("status"),
		index.Fields("sync_status"),
	}
}
