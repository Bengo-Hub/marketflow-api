package leads

import (
	eventslib "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Lead event names. The outbox publishes on "<aggregate>.<event>", so a new lead goes out on
// the "lead.created" subject that the nurture auto-enrol and lead scorer consumers subscribe to.
const (
	LeadAggregate    = "lead"
	LeadCreatedEvent = "created"
	LeadCreatedTopic = LeadAggregate + "." + LeadCreatedEvent
)

// LeadCreatedPayload is the payload of a lead.created event. Consumers read it with
// eventslib.DecodeEvent; the lead id is also the envelope's aggregate_id.
type LeadCreatedPayload struct {
	LeadID     string `json:"lead_id"`
	LeadType   string `json:"lead_type"`
	Source     string `json:"source"`
	CampaignID string `json:"campaign_id,omitempty"`
	FunnelID   string `json:"funnel_id,omitempty"`
}

// leadCreatedEvent builds the lead.created event for a saved lead. It holds no personal data
// (no name, email or phone): consumers load the lead by id when they need it.
func leadCreatedEvent(l *ent.Lead) *eventslib.Event {
	p := map[string]any{
		"lead_id":   l.ID.String(),
		"lead_type": l.LeadType,
		"source":    l.Source,
	}
	if l.CampaignID != nil {
		p["campaign_id"] = l.CampaignID.String()
	}
	if l.FunnelID != nil {
		p["funnel_id"] = l.FunnelID.String()
	}
	return eventslib.NewEvent(LeadCreatedEvent, LeadAggregate, l.ID, l.TenantID, p)
}
