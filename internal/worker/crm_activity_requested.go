package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
)

// CRMActivityRequestedSubject is the subject another service publishes when it wants an entry
// on a CRM timeline without calling the CRM directly. ticketing-api sends ticket_opened and
// ticket_resolved for the ticket's linked contact.
const CRMActivityRequestedSubject = "crm.activity.log_requested"

// activityEventIDKey is the metadata key holding the consumed event's id, the idempotency key.
const activityEventIDKey = "event_id"

// crmActivityRequest is the payload of crm.activity.log_requested. Only entity_type, entity_id
// and activity_type are required; the rest enrich the timeline entry.
type crmActivityRequest struct {
	TenantID     string `json:"tenant_id"`
	EntityType   string `json:"entity_type"`
	EntityID     string `json:"entity_id"`
	ActivityType string `json:"activity_type"`
	ExternalRef  string `json:"external_ref"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	Outcome      string `json:"outcome"`
	Direction    string `json:"direction"`
	Source       string `json:"source"`
}

// crmActivityRequested is a validated request ready to become an Activity.
type crmActivityRequested struct {
	eventID  string
	tenantID uuid.UUID
	entity   string
	entityID uuid.UUID
	at       time.Time
	source   string
	req      crmActivityRequest
}

// timelineEntities are the CRM records an Activity can hang off.
var timelineEntities = map[string]bool{"contact": true, "lead": true, "deal": true}

// decodeCRMActivityRequested reads the shared-events envelope. The tenant comes from the
// envelope (the payload's tenant_id is the fallback). The event id is the envelope id; an
// envelope without one gets a stable id hashed from the message, so a redelivery of the same
// bytes still lands once.
func decodeCRMActivityRequested(data []byte) (crmActivityRequested, error) {
	env, p, err := eventslib.DecodeEvent[crmActivityRequest](data)
	if err != nil {
		return crmActivityRequested{}, err
	}
	tenantID := env.TenantID
	if tenantID == uuid.Nil && p.TenantID != "" {
		if tenantID, err = uuid.Parse(p.TenantID); err != nil {
			return crmActivityRequested{}, fmt.Errorf("bad tenant_id %q", p.TenantID)
		}
	}
	if tenantID == uuid.Nil {
		return crmActivityRequested{}, errors.New("missing tenant_id")
	}
	entity := strings.ToLower(strings.TrimSpace(p.EntityType))
	if !timelineEntities[entity] {
		return crmActivityRequested{}, fmt.Errorf("unsupported entity_type %q", p.EntityType)
	}
	entityID, err := uuid.Parse(strings.TrimSpace(p.EntityID))
	if err != nil || entityID == uuid.Nil {
		return crmActivityRequested{}, fmt.Errorf("bad entity_id %q", p.EntityID)
	}
	p.ActivityType = strings.TrimSpace(p.ActivityType)
	if p.ActivityType == "" {
		return crmActivityRequested{}, errors.New("missing activity_type")
	}
	eventID := env.ID.String()
	if env.ID == uuid.Nil {
		eventID = uuid.NewSHA1(uuid.NameSpaceOID, data).String()
	}
	at := env.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	source := strings.TrimSpace(p.Source)
	if source == "" {
		source = env.AggregateType
	}
	return crmActivityRequested{
		eventID: eventID, tenantID: tenantID, entity: entity, entityID: entityID,
		at: at.UTC(), source: source, req: p,
	}, nil
}

// input maps the request onto the activities service, recording the event id in metadata.
func (r crmActivityRequested) input() activities.CreateInput {
	opt := func(s string) *string {
		if s = strings.TrimSpace(s); s == "" {
			return nil
		}
		return &s
	}
	outcome := opt(r.req.Outcome)
	if outcome == nil && r.req.ActivityType == "ticket_resolved" {
		outcome = opt("resolved")
	}
	source := r.source
	if source == "" {
		source = "cross_service"
	}
	at := r.at
	return activities.CreateInput{
		TenantID:     r.tenantID,
		EntityType:   r.entity,
		EntityID:     r.entityID,
		ActivityType: r.req.ActivityType,
		Direction:    opt(r.req.Direction),
		Subject:      opt(r.req.Subject),
		Body:         opt(r.req.Body),
		Outcome:      outcome,
		Source:       &source,
		ExternalRef:  opt(r.req.ExternalRef),
		PerformedAt:  &at,
		Metadata:     eventMetadata(r.eventID, CRMActivityRequestedSubject),
	}
}

// CRMActivityRequestedHandler consumes crm.activity.log_requested and writes the requested
// entry on the CRM timeline (a ticket opened or resolved on a contact, for one). It is
// idempotent on the event id: a redelivered event finds its earlier Activity and stops.
type CRMActivityRequestedHandler struct {
	db     *ent.Client
	svc    *activities.Service
	logger *zap.Logger
}

func (h *CRMActivityRequestedHandler) Subject() string { return CRMActivityRequestedSubject }
func (h *CRMActivityRequestedHandler) Durable() string {
	return "marketflow-worker-crm-activity-requested"
}

func (h *CRMActivityRequestedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	r, err := decodeCRMActivityRequested(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode "+CRMActivityRequestedSubject, err)
	}
	if h.db == nil {
		return nil
	}
	done, err := h.alreadyLogged(ctx, r)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	if _, err := h.svc.Log(ctx, r.input()); err != nil {
		h.logger.Error("failed to log requested CRM activity",
			zap.String("entity_type", r.entity), zap.String("entity_id", r.entityID.String()),
			zap.String("activity_type", r.req.ActivityType), zap.Error(err))
		return err
	}
	return nil
}

// alreadyLogged reports whether this event's Activity exists, keyed on metadata.event_id only:
// a ticket resolved, reopened and resolved again is two events with the same type and ref.
func (h *CRMActivityRequestedHandler) alreadyLogged(ctx context.Context, r crmActivityRequested) (bool, error) {
	return activityLogged(ctx, h.db, activityKey{
		tenantID: r.tenantID, entity: r.entity, entityID: r.entityID, eventID: r.eventID,
	})
}
