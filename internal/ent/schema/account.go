package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Account holds the schema definition for the Account entity.
// An account represents a company or organisation associated with contacts and deals.
type Account struct {
	ent.Schema
}

// Fields of the Account.
func (Account) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant"),

		field.String("name").
			NotEmpty().
			Comment("Company/organisation name"),
		field.String("industry").
			Optional().
			Nillable().
			Comment("tech | health | retail | finance | mlm | other"),
		field.String("website").
			Optional().
			Nillable(),
		field.String("phone").
			Optional().
			Nillable(),
		field.JSON("address", map[string]any{}).
			Optional().
			Comment("{street, city, country, postal_code}"),
		field.Int("employee_count").
			Optional().
			Nillable(),
		field.Float("annual_revenue").
			Optional().
			Nillable(),
		field.UUID("owner_user_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Assigned owner from auth-service"),

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

// Edges of the Account.
func (Account) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("accounts").
			Field("tenant_id").
			Required().
			Unique(),
		edge.To("contacts", Contact.Type),
		edge.To("deals", Deal.Type),
	}
}

// Indexes of the Account.
func (Account) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "name"),
		index.Fields("tenant_id", "industry"),
		index.Fields("owner_user_id"),
	}
}
