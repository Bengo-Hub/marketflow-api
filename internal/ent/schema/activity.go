package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Activity holds the schema definition for the Activity entity.
// An activity is any interaction or event logged against a lead, contact, or deal.
// entity_type + entity_id form a polymorphic reference so no typed FK edges are needed.
type Activity struct {
	ent.Schema
}

// Fields of the Activity.
func (Activity) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),

		field.String("entity_type").
			Comment("lead | contact | deal"),
		field.UUID("entity_id", uuid.UUID{}).
			Comment("Polymorphic FK to Lead, Contact, or Deal"),

		field.String("activity_type").
			Comment("email | call | note | meeting | sms | whatsapp | order | payment | ticket_opened | ticket_resolved | enrichment | appointment | pos_sale | suggestion"),
		field.String("direction").
			Optional().
			Nillable().
			Comment("inbound | outbound — for email/call/sms"),
		field.String("subject").
			Optional().
			Nillable().
			Comment("Email subject or call topic"),
		field.String("body").
			Optional().
			Nillable().
			Comment("Message body or note content"),
		field.String("outcome").
			Optional().
			Nillable().
			Comment("connected | voicemail | no_answer | booked | interested | not_interested"),
		field.Int("duration_minutes").
			Optional().
			Nillable().
			Comment("For calls and meetings"),

		field.UUID("performed_by", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("user_id from auth-service; nil = system or agent"),
		field.String("source").
			Optional().
			Nillable().
			Comment("manual | agent | cross_service"),
		field.String("external_ref").
			Optional().
			Nillable().
			Comment("External ID e.g. order_id, payment_id, ticket_id"),

		field.Time("performed_at").
			Default(time.Now).
			Comment("When the activity occurred"),
		field.Bool("is_read").
			Default(false).
			Comment("Marks suggestion activities as dismissed/read"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the Activity.
func (Activity) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("activities").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

// Indexes of the Activity.
func (Activity) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "entity_type", "entity_id"),
		index.Fields("tenant_id", "entity_type", "entity_id", "performed_at"),
		index.Fields("tenant_id", "activity_type"),
		index.Fields("external_ref"),
		index.Fields("performed_at"),
	}
}
