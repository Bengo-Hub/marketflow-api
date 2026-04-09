package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// OutboxEvent holds the schema definition for the OutboxEvent entity.
// Implements the transactional outbox pattern for reliable event publishing.
type OutboxEvent struct {
	ent.Schema
}

// Fields of the OutboxEvent.
func (OutboxEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("aggregate_type").
			NotEmpty().
			Comment("Entity type: lead, contact, campaign, etc."),
		field.UUID("aggregate_id", uuid.UUID{}).
			Comment("Entity ID"),
		field.String("event_type").
			NotEmpty().
			Comment("Event name: lead.created, contact.converted, etc."),
		field.Bytes("payload").
			Comment("JSON-serialized event payload"),
		field.String("status").
			Default("PENDING").
			Comment("PENDING | PUBLISHED | FAILED"),
		field.Int("attempts").
			Default(0),
		field.Time("last_attempt_at").
			Optional().
			Nillable(),
		field.Time("published_at").
			Optional().
			Nillable(),
		field.String("error_message").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the OutboxEvent.
func (OutboxEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status", "created_at"),
		index.Fields("tenant_id", "aggregate_type", "aggregate_id"),
	}
}
