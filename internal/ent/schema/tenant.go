package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Tenant holds the schema definition for the Tenant entity.
// Synced from auth-api; each tenant represents an FBO or business owner.
type Tenant struct {
	ent.Schema
}

// Fields of the Tenant.
func (Tenant) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty().
			Comment("Display name of the organisation or FBO"),
		field.String("slug").
			NotEmpty().
			Unique().
			Comment("URL-safe identifier; synced from auth-api"),
		field.String("status").
			Default("active").
			Comment("active | inactive | suspended"),

		// FBO-specific fields
		field.String("fbo_distrib_id").
			Optional().
			Nillable().
			Comment("FLP distributor ID (e.g., 254200047984) — used to construct shop/recruit links"),
		field.String("whatsapp_number").
			Optional().
			Nillable().
			Comment("WhatsApp number for wa.me deep links (E.164 format, no +)"),

		// FLP link construction fields (derived from distrib_id + store + language)
		// Shop link: https://shop.foreverliving.com/retail/entry/Shop.do?store={store}&language={lang}&distribID={distribID}
		// Product link: ...&itemCode={code}&distribID={distribID}
		// Category link: ...&categoryName={cat}&distribID={distribID}
		// Recruit link: https://join.foreverliving.com/marketing/joinnow/applicationForm.do?action=display&store={store}&distribID={distribID}&menuoff=n&langID={lang}
		// Profile link: https://marketflow.codevertexitsolutions.com/p/{tenant_slug}
		// Short links: https://marketflow.codevertexitsolutions.com/s/{code}
		field.String("flp_store_code").
			Optional().
			Nillable().
			Default("KEN").
			Comment("FLP country store code: KEN, USA, GBR, UGA, etc."),
		field.String("flp_language").
			Optional().
			Nillable().
			Default("en").
			Comment("FLP language code for links: en, fr, sw, etc."),
		field.String("flp_profile_uuid").
			Optional().
			Nillable().
			Comment("UUID from personalized FLP recruit profile page (e.g., 2c64be1f-f2cc-47a8-b357-39e9780d02e1)"),

		// Business config
		field.String("business_type").
			Default("flp").
			Comment("flp | generic — determines compliance rules and UI"),
		field.String("country").
			Optional().
			Nillable().
			Default("KE").
			Comment("ISO 3166-1 alpha-2 country code"),
		field.String("timezone").
			Optional().
			Nillable().
			Default("Africa/Nairobi"),

		// Meta integration
		field.String("meta_access_token").
			Optional().
			Nillable().
			Sensitive().
			Comment("Encrypted Facebook Marketing API access token"),
		field.String("meta_ad_account_id").
			Optional().
			Nillable().
			Comment("Facebook Ads account ID (act_XXXXX)"),
		field.String("meta_page_id").
			Optional().
			Nillable().
			Comment("Facebook Page ID linked to this tenant"),

		// Subscription (denormalized from subscriptions-service)
		field.String("subscription_plan").
			Optional().
			Nillable().
			Comment("STARTER | GROWTH | PRO"),
		field.String("subscription_status").
			Optional().
			Nillable().
			Comment("ACTIVE | TRIAL | EXPIRED | CANCELLED"),

		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("Free-form key-value store"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Tenant.
func (Tenant) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("leads", Lead.Type),
		edge.To("contacts", Contact.Type),
		edge.To("campaigns", Campaign.Type),
		edge.To("funnels", Funnel.Type),
		edge.To("knowledge_base_entries", KnowledgeBaseEntry.Type),
		edge.To("chat_sessions", ChatSession.Type),
		edge.To("nurture_sequences", NurtureSequence.Type),
		edge.To("nurture_enrollments", NurtureEnrollment.Type),
		edge.To("testimonials", Testimonial.Type),
		edge.To("profile_events", ProfileEvent.Type),
		edge.To("nurture_delivery_events", NurtureDeliveryEvent.Type),
		edge.To("ad_metrics", AdMetrics.Type),
		// Sprint 9 CRM entities
		edge.To("accounts", Account.Type),
		edge.To("pipelines", Pipeline.Type),
		edge.To("deals", Deal.Type),
		edge.To("activities", Activity.Type),
		edge.To("tasks", Task.Type),
		edge.To("custom_field_defs", CustomFieldDef.Type),
		edge.To("custom_field_values", CustomFieldValue.Type),
		edge.To("lead_scoring_rules", LeadScoringRule.Type),
		edge.To("custom_agents", CustomAgent.Type),
		edge.To("agent_runs", AgentRun.Type),
		edge.To("segments", Segment.Type),
		edge.To("behavioral_triggers", BehavioralTrigger.Type),
	}
}

// Indexes of the Tenant.
func (Tenant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug").Unique(),
		index.Fields("status"),
		index.Fields("business_type"),
		index.Fields("fbo_distrib_id").
			Unique().
			Annotations(entsql.IndexWhere("fbo_distrib_id IS NOT NULL AND fbo_distrib_id != ''")),
		index.Fields("whatsapp_number").
			Unique().
			Annotations(entsql.IndexWhere("whatsapp_number IS NOT NULL AND whatsapp_number != ''")),
	}
}
