package contacts

import (
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// ContactUpdatedTopic and ContactDeletedTopic are the subjects for identity changes and deletes.
const (
	ContactUpdatedTopic = "crm.contact.updated"
	ContactDeletedTopic = "crm.contact.deleted"
)

// contactUpdatedEvent names which identity fields changed (email, phone, first_name, last_name),
// never their values; consumers reload the contact.
func contactUpdatedEvent(c *ent.Contact, changed []string) *eventslib.Event {
	return eventslib.NewEvent("contact.updated", "crm", c.ID, c.TenantID, map[string]any{
		"contact_id": c.ID.String(),
		"fields":     changed,
	})
}

// contactDeletedEvent carries the id only.
func contactDeletedEvent(id, tenant uuid.UUID) *eventslib.Event {
	return eventslib.NewEvent("contact.deleted", "crm", id, tenant, map[string]any{
		"contact_id": id.String(),
	})
}

// ContactCreatedTopic is the subject a new contact is published on: the outbox
// publishes "<aggregate>.<event>", and the "crm" aggregate keeps it inside the
// worker stream's crm.> binding (like crm.contact.merged).
const ContactCreatedTopic = "crm.contact.created"

// contactCreatedEvent holds ids only, no personal data; consumers load the
// contact when they need it. Bulk imports do not emit it.
func contactCreatedEvent(c *ent.Contact) *eventslib.Event {
	p := map[string]any{
		"contact_id":   c.ID.String(),
		"contact_type": c.ContactType,
	}
	if c.LeadID != nil {
		p["lead_id"] = c.LeadID.String()
	}
	return eventslib.NewEvent("contact.created", "crm", c.ID, c.TenantID, p)
}
