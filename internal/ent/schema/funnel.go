package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Funnel holds the schema definition for the Funnel entity.
// A funnel is a multi-step landing page that captures leads and qualifies them.
type Funnel struct {
	ent.Schema
}

// Fields of the Funnel.
func (Funnel) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant"),

		field.String("name").
			NotEmpty().
			Comment("Funnel display name"),
		field.String("slug").
			NotEmpty().
			Comment("URL slug for /f/{slug}"),
		field.String("funnel_type").
			Default("product_inquiry").
			Comment("product_inquiry | recruitment | c9_challenge | generic"),
		field.String("status").
			Default("draft").
			Comment("draft | published | archived"),

		// Funnel structure
		field.JSON("steps", []map[string]any{}).
			Optional().
			Comment("Ordered array of funnel step configurations"),
		field.Text("landing_page_html").
			Optional().
			Comment("GrapesJS-generated HTML for the landing page"),
		field.JSON("landing_page_css", map[string]any{}).
			Optional().
			Comment("GrapesJS CSS and component data"),

		// Analytics
		field.Int("total_views").
			Default(0),
		field.Int("total_submissions").
			Default(0),
		field.Float("conversion_rate").
			Default(0).
			Comment("submissions / views * 100"),

		field.JSON("metadata", map[string]any{}).
			Optional(),

		field.Time("published_at").
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

// Edges of the Funnel.
func (Funnel) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("funnels").
			Field("tenant_id").
			Required().
			Unique(),
		edge.To("leads", Lead.Type),
	}
}

// Indexes of the Funnel.
func (Funnel) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "slug").Unique(),
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "funnel_type"),
	}
}
