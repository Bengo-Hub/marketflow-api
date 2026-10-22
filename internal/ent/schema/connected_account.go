package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ConnectedAccount is one external asset a tenant connected: a Facebook Page, an Instagram
// business account, a Meta ad account, a TikTok advertiser or creator, a YouTube channel or a
// Google Ads customer. A table, not ProviderSetting key/values, because a tenant has many
// accounts per platform. Tokens are stored only as KeyProvider ciphertext.
type ConnectedAccount struct {
	ent.Schema
}

// Fields of the ConnectedAccount.
func (ConnectedAccount) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("platform").
			NotEmpty().
			MaxLen(32).
			Comment("meta | tiktok | google"),
		field.String("account_type").
			NotEmpty().
			MaxLen(32).
			Comment("facebook_page | instagram_business | meta_ad_account | tiktok_creator | tiktok_advertiser | youtube_channel | google_ads_customer"),
		field.String("external_id").
			NotEmpty().
			MaxLen(128).
			Comment("Page id, act_<id>, advertiser id, channel id, customer id"),
		field.String("display_name").
			Optional().
			MaxLen(255),
		field.JSON("scopes", []string{}).
			Optional().
			Comment("Scopes granted at connect time"),
		field.Text("token_ciphertext").
			Optional().
			Sensitive().
			Comment("KeyProvider ciphertext; never logged or returned"),
		field.Text("refresh_ciphertext").
			Optional().
			Sensitive(),
		field.Time("expires_at").
			Optional().
			Nillable().
			Comment("Drives the token refresh job"),
		field.String("status").
			Default("active").
			MaxLen(16).
			Comment("active | expired | revoked | error"),
		field.UUID("connected_by", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("MarketFlow user who connected it"),
		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("currency, country, account_status, linked page_id, last funding check"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the ConnectedAccount.
func (ConnectedAccount) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("connected_accounts").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

// Indexes of the ConnectedAccount.
func (ConnectedAccount) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "platform", "account_type", "external_id").Unique(),
		index.Fields("tenant_id", "status"),
		// Webhooks find the tenant from a platform id (a Page id on a Lead Ads event).
		index.Fields("external_id", "account_type"),
		// The refresh job scans tokens about to expire across tenants.
		index.Fields("status", "expires_at"),
	}
}
