package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ChatSession holds the schema definition for the ChatSession entity.
// Represents an AI chatbot conversation with a lead or contact.
type ChatSession struct {
	ent.Schema
}

// Fields of the ChatSession.
func (ChatSession) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant"),

		field.UUID("lead_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("contact_id", uuid.UUID{}).
			Optional().
			Nillable(),

		field.String("status").
			Default("active").
			Comment("active | closed | escalated"),
		field.String("channel").
			Default("web_widget").
			Comment("web_widget | whatsapp | funnel"),

		// Conversation data
		field.JSON("messages", []map[string]any{}).
			Optional().
			Comment("Array of {role, content, timestamp, metadata}"),
		field.Int("message_count").
			Default(0),

		// AI performance
		field.Int("ai_responses").
			Default(0).
			Comment("Number of AI-generated responses"),
		field.Int("escalations").
			Default(0).
			Comment("Number of times escalated to human"),
		field.Float("avg_confidence").
			Optional().
			Nillable().
			Comment("Average AI confidence score for this session"),

		field.JSON("metadata", map[string]any{}).
			Optional(),

		field.Time("started_at").
			Default(time.Now).
			Immutable(),
		field.Time("ended_at").
			Optional().
			Nillable(),
	}
}

// Edges of the ChatSession.
func (ChatSession) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("chat_sessions").
			Field("tenant_id").
			Required().
			Unique(),
		edge.From("lead", Lead.Type).
			Ref("chat_sessions").
			Field("lead_id").
			Unique(),
		edge.From("contact", Contact.Type).
			Ref("chat_sessions").
			Field("contact_id").
			Unique(),
	}
}

// Indexes of the ChatSession.
func (ChatSession) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "lead_id"),
		index.Fields("tenant_id", "contact_id"),
		index.Fields("tenant_id", "channel"),
	}
}
