package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ProfileEvent is a visitor interaction on the public FBO profile page
// /p/{slug}. Recorded via POST /api/v1/public/profile-events with no auth —
// rate-limited + IP-hashed to avoid storing PII while still deduping.
type ProfileEvent struct {
	ent.Schema
}

func (ProfileEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("slug").
			MaxLen(255).
			Comment("Tenant slug of the profile viewed."),
		field.String("event_type").
			MaxLen(40).
			Comment("page_view | cta_click | whatsapp_click | chat_open | book_click | product_click"),
		field.String("referrer").
			Optional().
			MaxLen(1024),
		field.String("user_agent").
			Optional().
			MaxLen(512),
		field.String("ip_hash").
			Optional().
			MaxLen(64).
			Comment("SHA-256 of IP + daily salt; used to dedupe same-visitor events."),
		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("Optional event-specific payload — e.g. {\"cta\":\"join_my_team\"}"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (ProfileEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("profile_events").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

func (ProfileEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "event_type", "created_at"),
		index.Fields("slug", "created_at"),
	}
}
