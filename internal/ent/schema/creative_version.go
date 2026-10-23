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

// CreativeVersion is one poster or video made from a brief. Its id is the
// credit charge key (render:<id>) and names the stored file, so a retry never
// charges or stores twice.
type CreativeVersion struct {
	ent.Schema
}

// Fields of the CreativeVersion.
func (CreativeVersion) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("brief_id", uuid.UUID{}),
		field.UUID("parent_version_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("the version this one revises"),
		field.Int("version_no").
			Positive(),
		field.String("kind").
			Default("poster").
			Comment("poster | video"),
		field.String("tier").
			Default("standard").
			Comment("standard | premium"),
		field.String("format").
			Comment("1080x1080 | 1080x1350 | 1080x1920"),
		field.JSON("copy", map[string]any{}).
			Optional().
			Comment("prompt, headline, subline, price, cta"),
		field.String("status").
			Default("queued").
			Comment("queued | rendering | ready | failed | approved | rejected | superseded"),
		field.Int("credits").
			Default(0).
			Comment("credits charged by marketflow-ai"),
		field.String("error").
			Optional().
			MaxLen(500),
		field.UUID("approved_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("approved_at").
			Optional().
			Nillable(),
		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("route, cost_usd, attempts, scene_url (for text-only re-composes), photo_urls"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the CreativeVersion.
func (CreativeVersion) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("brief", CreativeBrief.Type).
			Ref("versions").
			Field("brief_id").
			Required().
			Unique(),
		edge.To("assets", CreativeAsset.Type),
	}
}

// Indexes of the CreativeVersion.
func (CreativeVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("brief_id", "version_no").Unique(),
		// Drafts waiting for review, and the per-tenant live-draft cap.
		index.Fields("tenant_id", "status", "created_at"),
		// The render queue: only the few rows in flight, whatever the table size.
		index.Fields("created_at").
			StorageKey("creativeversion_render_queue").
			Annotations(entsql.IndexWhere("status IN ('queued', 'rendering')")),
	}
}
