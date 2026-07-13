package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ProviderSetting holds the schema definition for the ProviderSetting entity.
// Stores platform-level and tenant-level provider configuration (AI models, Meta API, WhatsApp, etc.).
// Follows the same pattern as notifications-api ProviderSetting for consistency.
//
// Hierarchy (resolved by provider manager):
// 1. Platform-managed settings (is_platform_managed=true, tenant_id="platform")
// 2. Tenant-specific overrides (tenant_id=<actual UUID>)
// 3. Platform fallback settings (is_platform_managed=false, tenant_id="platform")
type ProviderSetting struct {
	ent.Schema
}

// Fields of the ProviderSetting.
func (ProviderSetting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),

		// "platform" for platform-level, actual tenant UUID string for tenant-level
		field.String("tenant_id").
			NotEmpty().
			Comment("'platform' for platform-level; tenant UUID for tenant-specific"),

		// Provider classification
		field.String("provider_type").
			NotEmpty().
			Comment("ai_llm | ai_embedding | meta_ads | whatsapp | email | scheduling"),
		field.String("provider_name").
			NotEmpty().
			Comment("groq | ollama | anthropic | meta | google_calendar | sendgrid | etc."),
		field.String("environment").
			Default("production").
			Comment("sandbox | production"),

		// Key-value setting
		field.String("key").
			NotEmpty().
			Comment("Setting key: api_key, base_url, model_id, temperature, etc."),
		field.Text("value").
			Optional().
			Comment("Setting value; encrypted if is_encrypted=true"),
		field.Text("description").
			Optional(),

		// Security & visibility flags
		field.Bool("is_encrypted").
			Default(false).
			Comment("Whether the value is AES-256-GCM encrypted at rest"),
		field.Bool("is_secret").
			Default(false).
			Comment("Whether value should be masked (••••••••) in API responses"),
		field.Bool("is_platform").
			Default(false).
			Comment("Platform-level setting (tenant_id='platform')"),
		field.Bool("is_platform_managed").
			Default(false).
			Comment("If true, only platform owners can manage; applies to all tenants"),
		field.Bool("is_active").
			Default(true).
			Comment("Whether this provider config is currently active"),

		field.String("status").
			Default("active").
			Comment("active | fallback | error | disabled"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the ProviderSetting.
func (ProviderSetting) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "environment", "provider_type", "provider_name", "key").
			Unique(),
		index.Fields("tenant_id", "environment", "provider_type"),
		index.Fields("tenant_id", "provider_type", "is_active"),
	}
}
