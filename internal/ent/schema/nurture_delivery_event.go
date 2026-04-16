package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// NurtureDeliveryEvent is an open/click/reply/bounce telemetry row emitted by
// notifications-api via the /webhooks/notifications/delivery sink. Used to
// compute per-sequence engagement metrics on the /sequences analytics page.
type NurtureDeliveryEvent struct {
	ent.Schema
}

func (NurtureDeliveryEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("enrollment_id", uuid.UUID{}),
		field.Int("step_index").
			Default(0),
		field.String("channel").
			MaxLen(20).
			Comment("email | sms | whatsapp | push"),
		field.String("event_type").
			MaxLen(24).
			Comment("sent | delivered | opened | clicked | bounced | replied | unsubscribed | failed"),
		field.String("provider_message_id").
			Optional().
			MaxLen(255).
			Comment("ID from the downstream provider (SendGrid messageId, Twilio SID, etc.)"),
		field.String("link_url").
			Optional().
			MaxLen(1024).
			Comment("Populated when event_type=clicked"),
		field.JSON("metadata", map[string]any{}).
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (NurtureDeliveryEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("nurture_delivery_events").
			Field("tenant_id").
			Required().
			Unique(),
		edge.From("enrollment", NurtureEnrollment.Type).
			Ref("delivery_events").
			Field("enrollment_id").
			Required().
			Unique(),
	}
}

func (NurtureDeliveryEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "event_type", "created_at"),
		index.Fields("enrollment_id", "step_index"),
		index.Fields("provider_message_id"),
	}
}
