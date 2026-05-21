package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Lead holds the schema definition for the Lead entity.
// A lead represents a potential customer or recruit captured through ads, funnels, or chat.
type Lead struct {
	ent.Schema
}

// Fields of the Lead.
func (Lead) Fields() []ent.Field {
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

		// Source tracking
		field.String("source").
			Default("organic").
			Comment("meta_ad | organic | referral | chatbot | whatsapp | manual"),
		field.UUID("campaign_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Campaign that generated this lead"),
		field.UUID("funnel_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Funnel that captured this lead"),
		field.String("utm_source").
			Optional().
			Nillable(),
		field.String("utm_medium").
			Optional().
			Nillable(),
		field.String("utm_campaign").
			Optional().
			Nillable(),

		// Lifecycle
		field.String("status").
			Default("new").
			Comment("new | contacted | qualified | converted | lost"),
		field.String("lead_type").
			Default("customer").
			Comment("customer | recruit — whether they're a product buyer or potential FBO"),
		field.Int("score").
			Default(0).
			Comment("Lead quality score (0-100)"),

		// Interest/qualification
		field.String("interest").
			Optional().
			Nillable().
			Comment("What brought them in: weight_management | energy | skincare | business_opportunity | other"),
		field.JSON("qualification_answers", map[string]any{}).
			Optional().
			Comment("Answers from funnel qualification steps"),

		// CRM fields (Sprint 9)
		field.UUID("owner_user_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Assigned owner user_id from auth-service"),

		// Lead scoring (Sprint 9)
		field.JSON("ai_score_breakdown", map[string]any{}).
			Optional().
			Comment("Scoring rule breakdown: [{rule_name, delta, matched}]"),

		// Enrichment (Sprint 10)
		field.JSON("enrichment_data", map[string]any{}).
			Optional().
			Comment("AI-enriched data from ContactEnricherAgent"),

		field.JSON("metadata", map[string]any{}).
			Optional(),

		field.Time("contacted_at").
			Optional().
			Nillable(),
		field.Time("qualified_at").
			Optional().
			Nillable(),
		field.Time("converted_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Lead.
func (Lead) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("leads").
			Field("tenant_id").
			Required().
			Unique(),
		edge.From("campaign", Campaign.Type).
			Ref("leads").
			Field("campaign_id").
			Unique(),
		edge.From("funnel", Funnel.Type).
			Ref("leads").
			Field("funnel_id").
			Unique(),
		edge.To("chat_sessions", ChatSession.Type),
		edge.To("scheduled_meetings", ScheduledMeeting.Type),
	}
}

// Indexes of the Lead.
func (Lead) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "email"),
		index.Fields("tenant_id", "phone"),
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "source"),
		index.Fields("tenant_id", "lead_type"),
		index.Fields("tenant_id", "created_at"),
	}
}
