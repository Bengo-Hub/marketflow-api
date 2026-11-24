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
	// FunnelID is the funnel the submission came through (uuid.Nil when none).
	FunnelID uuid.UUID
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
	out := leadCreated{TenantID: env.TenantID, LeadID: leadID, LeadType: p.LeadType, Source: p.Source}
	if f, err := uuid.Parse(p.FunnelID); err == nil {
		out.FunnelID = f
	}
	return out, nil
}

// decodeLeadResubmitted reads a lead.resubmitted message: a person the tenant already had
// submitted again (leads.Capture). Same shape as lead.created plus the submission activity.
func decodeLeadResubmitted(data []byte) (leadCreated, error) {
	env, p, err := eventslib.DecodeEvent[leads.LeadResubmittedPayload](data)
	if err != nil {
		return leadCreated{}, err
	}
	leadID, err := uuid.Parse(p.LeadID)
	if err != nil || env.TenantID == uuid.Nil {
		return leadCreated{}, errors.New("lead.resubmitted without tenant or lead id")
	}
	out := leadCreated{TenantID: env.TenantID, LeadID: leadID, LeadType: p.LeadType, Source: p.Source}
	if f, err := uuid.Parse(p.FunnelID); err == nil {
		out.FunnelID = f
	}
	return out, nil
}
