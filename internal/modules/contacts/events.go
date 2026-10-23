package contacts

import (
	eventslib "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/marketflow-api/internal/ent"
)

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
