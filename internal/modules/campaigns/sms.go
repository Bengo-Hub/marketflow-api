package campaigns

import (
	"context"
	"fmt"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	entcontact "github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// SendSMSInput holds the data needed to enqueue a bulk SMS for a campaign.
type SendSMSInput struct {
	TenantID   uuid.UUID
	CampaignID uuid.UUID
	SegmentID  *uuid.UUID
	Message    string
}

// SendSMS resolves the target contacts for a bulk SMS send and enqueues one
// `marketflow.campaign.sms_queued` event per contact via the transactional
// outbox. The shared-events OutboxPoller relays each row to NATS on the subject
// aggregate_type + "." + event_type ("marketflow.campaign.sms_queued"); the
// notifications-api consumer turns each event into an SMS delivery.
//
// Target resolution reuses the segment-member model when a segment is supplied,
// but the marketflow segment store only resolves a count today, so we resolve
// the concrete SMS-eligible contacts directly: tenant contacts that opted in to
// SMS marketing (subscribed_sms = true) and have a non-empty E.164 phone.
func (s *Service) SendSMS(ctx context.Context, in SendSMSInput) (int, error) {
	// Verify the campaign belongs to the tenant.
	camp, err := s.client.Campaign.Query().
		Where(campaign.ID(in.CampaignID), campaign.TenantID(in.TenantID)).
		Only(ctx)
	if err != nil {
		return 0, fmt.Errorf("load campaign: %w", err)
	}

	// Resolve SMS-eligible contacts for the tenant.
	contacts, err := s.client.Contact.Query().
		Where(
			entcontact.TenantID(in.TenantID),
			entcontact.SubscribedSms(true),
			entcontact.PhoneNotNil(),
			entcontact.PhoneNEQ(""),
		).
		All(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve sms contacts: %w", err)
	}

	queued := 0
	for _, c := range contacts {
		if c.Phone == nil || *c.Phone == "" {
			continue
		}
		payload := map[string]any{
			"tenant_id":   in.TenantID.String(),
			"campaign_id": camp.ID.String(),
			"contact_id":  c.ID.String(),
			"phone":       *c.Phone,
			"message":     in.Message,
		}

		// Build the canonical shared-events envelope. The OutboxPoller
		// deserializes the stored payload back into an events.Event and
		// publishes on event.Subject() == aggregate_type + "." + event_type.
		event := eventslib.NewEvent(
			"campaign.sms_queued", // event_type
			"marketflow",          // aggregate_type
			camp.ID,               // aggregate_id
			in.TenantID,
			payload,
		)
		raw, err := event.ToJSON()
		if err != nil {
			return queued, fmt.Errorf("marshal sms event: %w", err)
		}

		if _, err := s.client.OutboxEvent.Create().
			SetID(event.ID).
			SetTenantID(in.TenantID).
			SetAggregateType(event.AggregateType).
			SetAggregateID(event.AggregateID).
			SetEventType(event.EventType).
			SetPayload(raw).
			SetStatus(eventslib.StatusPending).
			Save(ctx); err != nil {
			return queued, fmt.Errorf("enqueue sms event: %w", err)
		}
		queued++
	}

	s.logger.Info("bulk sms queued",
		zap.String("campaign_id", camp.ID.String()),
		zap.String("tenant_id", in.TenantID.String()),
		zap.Int("queued", queued),
	)

	return queued, nil
}
