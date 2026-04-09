package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ServiceConfig holds the schema definition for the ServiceConfig entity.
// Platform-level and tenant-level configuration overrides (feature flags, limits, preferences).
// Follows the same pattern as notifications-api ServiceConfig.
type ServiceConfig struct {
	ent.Schema
}

// Fields of the ServiceConfig.
func (ServiceConfig) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),

		// Nil = platform-level default; set = tenant-specific override
		field.UUID("tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Nil = platform-level default; set = tenant-specific override"),

		field.String("config_key").
			NotEmpty().
			Comment("Configuration key, e.g., ai.default_model, chat.max_messages"),
		field.Text("config_value").
			NotEmpty().
			Comment("Configuration value as JSON string"),
		field.String("config_type").
			Default("string").
			Comment("Value type: string | int | bool | json | float"),
		field.String("description").
			Optional(),
		field.Bool("is_secret").
			Default(false).
			Comment("If true, value is masked in API responses"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the ServiceConfig.
func (ServiceConfig) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "config_key").Unique(),
		index.Fields("config_key"),
	}
}
