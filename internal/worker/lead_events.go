package worker

import (
	"errors"
	"fmt"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/modules/leads"
)

// leadCreated is a decoded lead.created event, shared by every consumer of that subject.
type leadCreated struct {
	TenantID uuid.UUID
	LeadID   uuid.UUID
	LeadType string
	Source   string
}

// decodeLeadCreated reads a lead.created message from the shared-events envelope. The tenant
// comes from the envelope; the lead id from the payload, falling back to the aggregate id.
func decodeLeadCreated(data []byte) (leadCreated, error) {
	env, p, err := eventslib.DecodeEvent[leads.LeadCreatedPayload](data)
	if err != nil {
		return leadCreated{}, err
	}
	if env.TenantID == uuid.Nil {
		return leadCreated{}, errors.New("lead.created without tenant_id")
	}
	leadID := env.AggregateID
	if p.LeadID != "" {
		parsed, err := uuid.Parse(p.LeadID)
		if err != nil {
			return leadCreated{}, fmt.Errorf("invalid lead_id: %w", err)
		}
		leadID = parsed
	}
	if leadID == uuid.Nil {
		return leadCreated{}, errors.New("lead.created without lead id")
	}
	return leadCreated{TenantID: env.TenantID, LeadID: leadID, LeadType: p.LeadType, Source: p.Source}, nil
}
