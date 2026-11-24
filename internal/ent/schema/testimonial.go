package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Testimonial is a success-story / quote rendered on the FBO profile page
// /p/{slug}. Tenants (FBOs) author them via the dashboard settings.
type Testimonial struct {
	ent.Schema
}

func (Testimonial) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("author_name").
			NotEmpty().
			MaxLen(120),
		field.String("author_role").
			Optional().
			MaxLen(120).
			Comment("e.g. Customer, Team member, Distributor"),
		field.String("body").
			NotEmpty().
			Comment("The quote itself. Markdown allowed."),
		field.String("avatar_url").
			Optional().
			MaxLen(512),
		field.Int("rating").
			Default(5).
			Min(1).
			Max(5),
		field.Int("display_order").
			Default(0).
			Comment("Lower = shown first. Used to sort testimonials on /p/{slug}."),
		field.Bool("is_published").
			Default(true),
		// Review state. Rows the tenant writes itself are approved; a customer's form or a quote
		// found on the tenant's website waits for an admin. Only approved rows can be published.
		field.String("status").
			Default("approved").
			Comment("pending | approved | rejected"),
		field.String("source").
			Default("manual").
			Comment("manual | form | website"),
		// Fingerprint is a hash of the normalised author and words, so the same testimonial sent
		// twice or found again on the website is stored once per tenant.
		field.String("fingerprint").
			Optional().
			Nillable().
			MaxLen(64),
		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("consent, consent_at, submitter_email, submitter_phone, media_url, media_kind, source_url, reviewed_by, reviewed_at, ip_hash"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (Testimonial) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("testimonials").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

func (Testimonial) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "is_published", "display_order"),
		index.Fields("tenant_id", "created_at"),
		index.Fields("tenant_id", "status", "created_at"),
		index.Fields("tenant_id", "fingerprint").Unique(),
	}
}
