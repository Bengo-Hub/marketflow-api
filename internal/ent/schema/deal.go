package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Deal holds the schema definition for the Deal entity.
// A deal tracks a sales opportunity moving through a pipeline.
type Deal struct {
	ent.Schema
}

// Fields of the Deal.
func (Deal) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),

		field.String("title").
			NotEmpty().
			Comment("Short deal label"),

		field.UUID("contact_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Primary CRM contact"),
		field.UUID("lead_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Pre-conversion lead"),
		field.UUID("account_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Associated company account"),
		field.UUID("pipeline_id", uuid.UUID{}).
			Comment("Sales pipeline this deal belongs to"),
		field.UUID("stage_id", uuid.UUID{}).
			Comment("Current pipeline stage"),

		field.Float("value").
			Optional().
			Nillable().
			Comment("Monetary value of the deal"),
		field.String("currency").
			Default("USD").
			Comment("ISO 4217 currency code"),
		field.Float("probability").
			Optional().
			Nillable().
			Comment("Override probability 0.0-1.0; nil = use stage default"),
		field.Time("expected_close_date").
			Optional().
			Nillable(),

		field.String("status").
			Default("open").
			Comment("open | won | lost"),
		field.String("lost_reason").
			Optional().
			Nillable(),
		field.UUID("owner_user_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Assigned sales owner from auth-service"),

		field.JSON("metadata", map[string]any{}).
			Optional(),

		field.Time("won_at").
			Optional().
			Nillable(),
		field.Time("lost_at").
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

// Edges of the Deal.
func (Deal) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("deals").
			Field("tenant_id").
			Required().
			Unique(),
		edge.From("contact", Contact.Type).
			Ref("deals").
			Field("contact_id").
			Unique(),
		edge.From("account", Account.Type).
			Ref("deals").
			Field("account_id").
			Unique(),
		edge.From("pipeline", Pipeline.Type).
			Ref("deals").
			Field("pipeline_id").
			Required().
			Unique(),
		edge.From("stage", PipelineStage.Type).
			Ref("deals").
			Field("stage_id").
			Required().
			Unique(),
		// Activities and Tasks are queried polymorphically (entity_type="deal", entity_id=deal.id)
		// so no typed Ent edge is needed here.
	}
}

// Indexes of the Deal.
func (Deal) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "contact_id"),
		index.Fields("tenant_id", "pipeline_id"),
		index.Fields("tenant_id", "stage_id"),
		index.Fields("tenant_id", "owner_user_id"),
		index.Fields("tenant_id", "expected_close_date"),
		index.Fields("tenant_id", "created_at"),
	}
}
