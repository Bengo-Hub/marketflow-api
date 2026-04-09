package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ShortLink holds the schema definition for the ShortLink entity.
// Provides URL shortening for all generated links (product, recruit, profile, category, WhatsApp).
// Short URLs resolve at GET /s/{code} which 302-redirects to the full target URL.
// Tracks click analytics (count, last clicked, referrer, device).
type ShortLink struct {
	ent.Schema
}

// Fields of the ShortLink.
func (ShortLink) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant"),

		// Short code — the unique alphanumeric identifier in the URL
		// e.g., "abc123" → /s/abc123
		field.String("code").
			NotEmpty().
			Unique().
			Comment("Short alphanumeric code (6-8 chars)"),

		// Target URL — the full URL this short link resolves to
		field.Text("target_url").
			NotEmpty().
			Comment("Full target URL to redirect to"),

		// Classification
		field.String("link_type").
			Default("product").
			Comment("product | category | recruit | profile | funnel | whatsapp | custom"),

		// Optional metadata for tracking
		field.String("title").
			Optional().
			Nillable().
			Comment("Friendly label for the link (e.g., product name)"),
		field.UUID("campaign_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Associated campaign for attribution"),

		// Analytics
		field.Int("click_count").
			Default(0),
		field.Time("last_clicked_at").
			Optional().
			Nillable(),

		// Expiration
		field.Time("expires_at").
			Optional().
			Nillable().
			Comment("Optional expiration; nil = never expires"),
		field.Bool("is_active").
			Default(true),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the ShortLink.
func (ShortLink) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("code").Unique(),
		index.Fields("tenant_id", "link_type"),
		index.Fields("tenant_id", "campaign_id"),
		index.Fields("tenant_id", "created_at"),
	}
}
