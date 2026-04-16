package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Campaign holds the schema definition for the Campaign entity.
// Represents a Meta (Facebook/Instagram) ad campaign or manual marketing campaign.
type Campaign struct {
	ent.Schema
}

// Fields of the Campaign.
func (Campaign) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant (platform-owner tenant for scope='platform' templates)"),
		field.String("scope").
			Default("tenant").
			Comment("tenant = tenant-owned campaign | platform = shared template every tenant can clone (lead-gen, retargeting, recruitment starter templates)"),

		field.String("name").
			NotEmpty().
			Comment("Campaign display name"),
		field.String("campaign_type").
			Default("lead_gen").
			Comment("lead_gen | retargeting | recruitment | re_engagement"),
		field.String("platform").
			Default("meta").
			Comment("meta | manual | google"),
		field.String("status").
			Default("draft").
			Comment("draft | active | paused | completed | failed"),

		// Meta API references
		field.String("meta_campaign_id").
			Optional().
			Nillable().
			Comment("Facebook campaign ID"),
		field.String("meta_adset_id").
			Optional().
			Nillable().
			Comment("Facebook ad set ID"),
		field.String("meta_ad_id").
			Optional().
			Nillable().
			Comment("Facebook ad ID"),

		// Budget
		field.Float("budget_daily").
			Optional().
			Nillable().
			Comment("Daily budget in tenant's currency"),
		field.Float("budget_total").
			Optional().
			Nillable().
			Comment("Total campaign budget"),
		field.String("currency").
			Default("USD"),

		// Targeting
		field.JSON("target_audience", map[string]any{}).
			Optional().
			Comment("Age range, location, interests, etc."),
		field.UUID("funnel_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Landing funnel for this campaign"),

		// Schedule
		field.Time("start_date").
			Optional().
			Nillable(),
		field.Time("end_date").
			Optional().
			Nillable(),

		// Performance metrics (synced from Meta API)
		field.JSON("metrics", map[string]any{}).
			Optional().
			Comment("impressions, clicks, leads, cost, ctr, cpl, etc."),

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

// Edges of the Campaign.
func (Campaign) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("campaigns").
			Field("tenant_id").
			Required().
			Unique(),
		edge.To("leads", Lead.Type),
	}
}

// Indexes of the Campaign.
func (Campaign) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "campaign_type"),
		index.Fields("tenant_id", "platform"),
		index.Fields("meta_campaign_id"),
	}
}
