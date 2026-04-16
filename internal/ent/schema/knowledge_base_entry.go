package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// KnowledgeBaseEntry holds the schema definition for the KnowledgeBaseEntry entity.
// Stores product information, FAQs, and compliance-approved content for RAG retrieval.
type KnowledgeBaseEntry struct {
	ent.Schema
}

// Fields of the KnowledgeBaseEntry.
func (KnowledgeBaseEntry) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant (platform-owner tenant for scope='platform' rows)"),
		field.String("scope").
			Default("tenant").
			Comment("tenant = private to tenant_id | platform = shared across every tenant (RAG base knowledge)"),

		field.String("category").
			Default("product").
			Comment("product | faq | compliance | business_opportunity | custom"),
		field.String("title").
			NotEmpty(),
		field.Text("content").
			NotEmpty().
			Comment("Main content body (markdown or plain text)"),

		// Product-specific fields
		field.String("product_code").
			Optional().
			Nillable().
			Comment("FLP product code if applicable"),
		field.Float("product_price").
			Optional().
			Nillable().
			Comment("Retail price in tenant's currency"),
		field.Float("product_cc").
			Optional().
			Nillable().
			Comment("Case credit value"),
		field.String("product_image_url").
			Optional().
			Nillable(),

		// Embedding status
		field.Bool("embedded").
			Default(false).
			Comment("Whether vector embeddings have been generated"),
		field.Int("chunk_count").
			Default(0).
			Comment("Number of embedding chunks generated"),

		field.JSON("tags", []string{}).
			Optional().
			Comment("Searchable tags"),
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

// Edges of the KnowledgeBaseEntry.
func (KnowledgeBaseEntry) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("knowledge_base_entries").
			Field("tenant_id").
			Required().
			Unique(),
	}
}

// Indexes of the KnowledgeBaseEntry.
func (KnowledgeBaseEntry) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "category"),
		index.Fields("tenant_id", "product_code"),
		index.Fields("tenant_id", "embedded"),
		// Platform-scoped rows are loaded into every tenant's RAG query, so
		// scope+category is a hot index for agent_tool.kb_search.
		index.Fields("scope", "category"),
	}
}
