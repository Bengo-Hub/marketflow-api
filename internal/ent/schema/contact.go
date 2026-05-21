package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Contact holds the schema definition for the Contact entity.
// A contact is a known customer or business partner, potentially converted from a lead.
type Contact struct {
	ent.Schema
}

// Fields of the Contact.
func (Contact) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant"),

		// Contact info
		field.String("email").
			Optional().
			Nillable(),
		field.String("phone").
			Optional().
			Nillable().
			Comment("E.164 format"),
		field.String("first_name").
			Optional().
			Nillable(),
		field.String("last_name").
			Optional().
			Nillable(),

		// Type and lifecycle
		field.String("contact_type").
			Default("customer").
			Comment("customer | prospect | recruit | downline"),
		field.UUID("lead_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Original lead if converted"),

		// FLP-specific
		field.String("flp_customer_id").
			Optional().
			Nillable().
			Comment("FLP customer ID in Forever's system"),
		field.Float("total_cc").
			Default(0).
			Comment("Total case credits purchased"),
		field.Int("total_orders").
			Default(0),
		field.Time("last_order_at").
			Optional().
			Nillable(),

		// CRM fields (Sprint 9)
		field.UUID("account_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Linked company/account"),
		field.UUID("owner_user_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Assigned owner user_id from auth-service"),
		field.String("lifecycle_stage").
			Default("lead").
			Comment("subscriber | lead | prospect | customer | evangelist"),

		// Enrichment (Sprint 10)
		field.JSON("enrichment_data", map[string]any{}).
			Optional().
			Comment("AI-enriched company/LinkedIn data from ContactEnricherAgent"),

		// Preferences
		field.JSON("product_interests", []string{}).
			Optional().
			Comment("Product categories of interest"),
		field.Bool("subscribed_email").
			Default(true).
			Comment("Opted in for email marketing"),
		field.Bool("subscribed_sms").
			Default(true).
			Comment("Opted in for SMS marketing"),

		field.JSON("metadata", map[string]any{}).
			Optional(),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Contact.
func (Contact) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("contacts").
			Field("tenant_id").
			Required().
			Unique(),
		edge.To("chat_sessions", ChatSession.Type),
		edge.To("scheduled_meetings", ScheduledMeeting.Type),
		// Sprint 9 CRM edges
		edge.From("account", Account.Type).
			Ref("contacts").
			Field("account_id").
			Unique(),
		edge.To("deals", Deal.Type),
	}
}

// Indexes of the Contact.
func (Contact) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "email"),
		index.Fields("tenant_id", "phone"),
		index.Fields("tenant_id", "contact_type"),
		index.Fields("tenant_id", "last_order_at"),
		index.Fields("tenant_id", "lifecycle_stage"),
		index.Fields("tenant_id", "owner_user_id"),
		index.Fields("account_id"),
	}
}
