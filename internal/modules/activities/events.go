package activities

import (
	eventslib "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// ActivityLoggedTopic is the subject a new timeline entry is published on. The outbox publishes
// "<aggregate>.<event>", and the "crm" aggregate keeps it inside the worker stream's crm.>
// binding. Consumers today: the lead scorer (entity lead) and the contact enricher (entity
// contact). Neither writes timeline entries, so neither can trigger itself.
const ActivityLoggedTopic = "crm.activity.logged"

// activityLoggedEvent carries ids and kinds only, never the subject or body; consumers load what
// they need.
func activityLoggedEvent(a *ent.Activity) *eventslib.Event {
	p := map[string]any{
		"activity_id":   a.ID.String(),
		"entity_type":   a.EntityType,
		"entity_id":     a.EntityID.String(),
		"activity_type": a.ActivityType,
	}
	if a.Source != nil {
		p["source"] = *a.Source
	}
	return eventslib.NewEvent("activity.logged", "crm", a.ID, a.TenantID, p)
}
