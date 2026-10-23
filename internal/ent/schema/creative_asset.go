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

// CreativeAsset is a stored file of a version (the poster JPEG, later the video,
// its WhatsApp rendition and thumbnail) in shared object storage.
type CreativeAsset struct {
	ent.Schema
}

// Fields of the CreativeAsset.
func (CreativeAsset) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("version_id", uuid.UUID{}),
		field.String("kind").
			Default("image").
			Comment("image | video | wa_rendition | thumbnail"),
		field.String("storage_key").
			NotEmpty().
			Comment("httpware/objectstore key, e.g. marketflow/<tenant>/creative-drafts/posters/<version>.jpg"),
		field.String("url").
			NotEmpty(),
		field.Int64("bytes").
			Default(0),
		field.Int("width").
			Default(0),
		field.Int("height").
			Default(0),
		field.Int("duration_ms").
			Default(0),
		field.String("sha256").
			Optional(),
		field.Time("expires_at").
			Optional().
			Nillable().
			Comment("draft lifetime; cleared when the version is approved"),
		field.Time("purged_at").
			Optional().
			Nillable(),
		field.JSON("metadata", map[string]any{}).
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the CreativeAsset.
func (CreativeAsset) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("version", CreativeVersion.Type).
			Ref("assets").
			Field("version_id").
			Required().
			Unique(),
	}
}

// Indexes of the CreativeAsset.
func (CreativeAsset) Indexes() []ent.Index {
	return []ent.Index{
		// The hourly purge reads only live expiring files.
		index.Fields("expires_at").
			Annotations(entsql.IndexWhere("purged_at IS NULL AND expires_at IS NOT NULL")),
		index.Fields("tenant_id", "version_id"),
	}
}
