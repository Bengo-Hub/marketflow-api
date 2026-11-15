package worker

import (
	"context"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/nurturedelivery"
)

// NurtureEngagementSubject is where notifications-api reports how a nurture message did: sent,
// delivered, opened (a WhatsApp read), bounced, replied or failed (its package engagement).
const NurtureEngagementSubject = "notifications.engagement.recorded"

// NurtureEngagementHandler records each engagement event as a NurtureDeliveryEvent, the rows
// the sequence analytics count. Idempotent on the event id.
type NurtureEngagementHandler struct {
	db     *ent.Client
	logger *zap.Logger
}

func (h *NurtureEngagementHandler) Subject() string { return NurtureEngagementSubject }
func (h *NurtureEngagementHandler) Durable() string {
	return "marketflow-worker-nurture-engagement"
}

// decodeNurtureEngagement reads the envelope: the payload is the delivery input, the envelope id
// the idempotency key (a stable hash of the message when the id is missing).
func decodeNurtureEngagement(data []byte) (nurturedelivery.Validated, string, error) {
	env, in, err := eventslib.DecodeEvent[nurturedelivery.Input](data)
	if err != nil {
		return nurturedelivery.Validated{}, "", err
	}
	if in.TenantID == "" && env.TenantID != uuid.Nil {
		in.TenantID = env.TenantID.String()
	}
	v, err := nurturedelivery.Validate(in)
	if err != nil {
		return nurturedelivery.Validated{}, "", err
	}
	id := env.ID.String()
	if env.ID == uuid.Nil {
		id = uuid.NewSHA1(uuid.NameSpaceOID, data).String()
	}
	return v, id, nil
}

func (h *NurtureEngagementHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	v, eventID, err := decodeNurtureEngagement(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode "+NurtureEngagementSubject, err)
	}
	if v.Skip || h.db == nil {
		return nil
	}
	if _, err := nurturedelivery.Record(ctx, h.db, v, eventID); err != nil {
		// The enrollment is gone (deleted with its lead or tenant): nothing left to count against.
		if ent.IsConstraintError(err) {
			return dropPoison(h.logger, msg, "enrollment no longer exists", err)
		}
		return err
	}
	return nil
}
