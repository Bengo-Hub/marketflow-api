// Package nurturedelivery records how a nurture message did after it left MarketFlow: sent,
// delivered, opened, clicked, bounced, replied, unsubscribed or failed. notifications-api reports
// these on notifications.engagement.recorded (the worker consumer) and, for older senders, on
// POST /webhooks/notifications/delivery. Both paths go through Record.
package nurturedelivery

import (
	"context"
	"strings"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/nurturedeliveryevent"
)

// EventIDKey is the metadata key holding the consumed event's id, the idempotency key.
const EventIDKey = "event_id"

// Input is one delivery event, in the shape notifications-api sends.
type Input struct {
	TenantID          string         `json:"tenant_id"`
	EnrollmentID      string         `json:"enrollment_id,omitempty"`
	StepIndex         int            `json:"step_index,omitempty"`
	Channel           string         `json:"channel"`
	EventType         string         `json:"event_type"`
	ProviderMessageID string         `json:"provider_message_id,omitempty"`
	LinkURL           string         `json:"link_url,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

// ValidationError is a request that can never be recorded. Code is the API error code.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

var validEvents = map[string]struct{}{
	"sent": {}, "delivered": {}, "opened": {}, "clicked": {},
	"bounced": {}, "replied": {}, "unsubscribed": {}, "failed": {},
}

// Validated is an Input ready to save. Skip is true for a message that is not a nurture step
// (no enrollment_id): nothing to record, and not an error.
type Validated struct {
	Input
	Tenant     uuid.UUID
	Enrollment uuid.UUID
	Skip       bool
}

// Validate normalises and checks an input.
func Validate(in Input) (Validated, error) {
	in.EventType = strings.ToLower(strings.TrimSpace(in.EventType))
	in.Channel = strings.ToLower(strings.TrimSpace(in.Channel))
	if in.TenantID == "" || in.EventType == "" {
		return Validated{}, &ValidationError{"missing_fields", "tenant_id and event_type are required"}
	}
	if _, ok := validEvents[in.EventType]; !ok {
		return Validated{}, &ValidationError{"unknown_event_type", "unknown event_type"}
	}
	tenantID, err := uuid.Parse(in.TenantID)
	if err != nil {
		return Validated{}, &ValidationError{"invalid_tenant_id", err.Error()}
	}
	if in.EnrollmentID == "" {
		return Validated{Input: in, Tenant: tenantID, Skip: true}, nil
	}
	enrollmentID, err := uuid.Parse(in.EnrollmentID)
	if err != nil {
		return Validated{}, &ValidationError{"invalid_enrollment_id", err.Error()}
	}
	if in.Channel == "" {
		in.Channel = "email"
	}
	return Validated{Input: in, Tenant: tenantID, Enrollment: enrollmentID}, nil
}

// Record saves a validated event. With an eventID it is idempotent: the id is kept in metadata
// and an event already recorded is not saved again (recorded is then false).
func Record(ctx context.Context, client *ent.Client, v Validated, eventID string) (recorded bool, err error) {
	if v.Skip {
		return false, nil
	}
	meta := map[string]any{}
	for k, val := range v.Metadata {
		meta[k] = val
	}
	if eventID != "" {
		exists, err := client.NurtureDeliveryEvent.Query().
			Where(
				nurturedeliveryevent.EnrollmentID(v.Enrollment),
				func(s *sql.Selector) {
					s.Where(sqljson.ValueEQ(nurturedeliveryevent.FieldMetadata, eventID, sqljson.Path(EventIDKey)))
				},
			).
			Exist(ctx)
		if err != nil {
			return false, err
		}
		if exists {
			return false, nil
		}
		meta[EventIDKey] = eventID
	}
	b := client.NurtureDeliveryEvent.Create().
		SetTenantID(v.Tenant).
		SetEnrollmentID(v.Enrollment).
		SetStepIndex(v.StepIndex).
		SetChannel(truncate(v.Channel, 20)).
		SetEventType(v.EventType)
	if v.ProviderMessageID != "" {
		b.SetProviderMessageID(truncate(v.ProviderMessageID, 255))
	}
	if v.LinkURL != "" {
		b.SetLinkURL(truncate(v.LinkURL, 1024))
	}
	if len(meta) > 0 {
		b.SetMetadata(meta)
	}
	if _, err := b.Save(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
