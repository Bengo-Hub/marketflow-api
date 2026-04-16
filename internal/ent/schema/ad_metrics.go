package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AdMetrics is a daily rollup of ad-platform insights (impressions, clicks,
// spend, leads, CTR, CPL) for a single campaign. Populated by the insights
// polling worker that runs every 15 minutes against Meta / Google Ads /
// TikTok / LinkedIn / X APIs.
type AdMetrics struct {
	ent.Schema
}

func (AdMetrics) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("campaign_id", uuid.UUID{}),
		field.String("platform").
			MaxLen(32).
			Comment("meta | google | tiktok | linkedin | x | manual"),
		field.String("external_campaign_id").
			Optional().
			MaxLen(128).
			Comment("Platform-side campaign id (e.g. Meta campaign_id)"),
		field.Time("date").
			Comment("UTC calendar day this metrics row represents."),
		field.Int64("impressions").
			Default(0),
		field.Int64("clicks").
			Default(0),
		field.Int64("leads").
			Default(0),
		field.Int64("conversions").
			Default(0),
		field.Float("spend").
			Default(0).
			Comment("Amount spent in currency units."),
		field.Float("ctr").
			Default(0).
			Comment("Computed: clicks / impressions."),
		field.Float("cpl").
			Default(0).
			Comment("Computed: spend / leads (or 0 when leads=0)."),
		field.String("currency").
			Default("USD").
			MaxLen(8),
		field.JSON("raw", map[string]any{}).
			Optional().
			Comment("Full provider response payload for troubleshooting."),
		field.Time("fetched_at").
			Default(time.Now),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (AdMetrics) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("ad_metrics").
			Field("tenant_id").
			Required().
			Unique(),
		edge.From("campaign", Campaign.Type).
			Ref("ad_metrics").
			Field("campaign_id").
			Required().
			Unique(),
	}
}

func (AdMetrics) Indexes() []ent.Index {
	return []ent.Index{
		// Upsert key: one row per (campaign, date) ensures safe re-fetch.
		index.Fields("campaign_id", "date").Unique(),
		index.Fields("tenant_id", "platform", "date"),
		index.Fields("tenant_id", "date"),
	}
}
