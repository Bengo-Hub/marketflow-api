package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AuditLog captures events for compliance + platform visibility.
// Written by the worker's ChatCompletedHandler (chat.completed), the admin
// provider-configure handler (admin.provider_updated), and any other
// security-relevant action.
//
// Two visibility modes via `scope`:
//   - "tenant" — visible in the tenant dashboard / compliance export
//   - "platform" — only surfaced on /platform/security
type AuditLog struct {
	ent.Schema
}

func (AuditLog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Owning tenant; nil for platform-scope events"),
		field.String("scope").
			Default("tenant").
			Comment("tenant | platform"),
		field.String("event").
			NotEmpty().
			Comment("chat.completed | admin.provider_updated | lead.created | ..."),
		field.String("actor").
			Optional().
			Nillable().
			Comment("user id, service name (marketflow-worker), or 'system'"),
		field.String("subject_type").
			Optional().
			Nillable().
			Comment("lead | chat_session | provider_setting | tenant"),
		field.UUID("subject_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Primary subject of the event, if any"),
		field.JSON("payload", map[string]any{}).
			Optional().
			Comment("Full event payload as received"),
		field.Time("occurred_at").
			Default(time.Now).
			Immutable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (AuditLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "event"),
		index.Fields("scope", "event"),
		index.Fields("occurred_at"),
	}
}
