package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CreativeBrief is what an ad is for (Sprint 15, Creative Studio): the offering,
// the objective and the audience. Its versions are the posters and videos made
// from it.
type CreativeBrief struct {
	ent.Schema
}

// Fields of the CreativeBrief.
func (CreativeBrief) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("title").
			NotEmpty().
			MaxLen(160),
		field.JSON("offering", map[string]any{}).
			Optional().
			Comment("snapshot: type, source, source_id, name, price, currency, url, photo_urls (never owned data)"),
		field.String("objective").
			Default("sales").
			Comment("sales | leads | awareness | traffic"),
		field.String("audience").
			Optional().
			MaxLen(500),
		field.String("language").
			Default("en").
			Comment("en by default; sw when the brief asks"),
		field.String("status").
			Default("active").
			Comment("active | archived"),
		field.UUID("created_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("plan (Creative Plan), brand snapshot (accent, logo_url, palette), testimonial ad-use consent"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the CreativeBrief.
func (CreativeBrief) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("versions", CreativeVersion.Type),
	}
}

// Indexes of the CreativeBrief.
func (CreativeBrief) Indexes() []ent.Index {
	return []ent.Index{
		// The studio list: a tenant's briefs by status, newest first.
		index.Fields("tenant_id", "status", "created_at"),
	}
}
