package events

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
)

// UnsubscribeConsumer clears a contact's marketing consent when the person opts out from a
// message (unsubscribe link, STOP reply, "Stop promotions" button) handled by notifications-api,
// so MarketFlow's own campaigns honour it too.
type UnsubscribeConsumer struct {
	db     *ent.Client
	logger *zap.Logger
	sub    *nats.Subscription
}

// NewUnsubscribeConsumer builds the consumer.
func NewUnsubscribeConsumer(db *ent.Client, logger *zap.Logger) *UnsubscribeConsumer {
	return &UnsubscribeConsumer{db: db, logger: logger.Named("unsubscribe.consumer")}
}

// Start subscribes (one replica handles each event).
func (c *UnsubscribeConsumer) Start(conn *nats.Conn) error {
	sub, err := eventslib.QueueSubscribe(c.logger, conn, "notifications.recipient.unsubscribed", "marketflow-unsubscribe", c.handle)
	if err != nil {
		return err
	}
	c.sub = sub
	return nil
}

// Stop unsubscribes.
func (c *UnsubscribeConsumer) Stop() {
	if c.sub != nil {
		_ = c.sub.Unsubscribe()
	}
}

func (c *UnsubscribeConsumer) handle(msg *nats.Msg) {
	var ev struct {
		Payload struct {
			TenantID string `json:"tenant_id"`
			Channel  string `json:"channel"`
			Address  string `json:"address"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		c.logger.Warn("unsubscribe event: bad payload", zap.Error(err))
		return
	}
	p := ev.Payload
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil || strings.TrimSpace(p.Address) == "" {
		return // a platform opt-out, or no address to match: nothing of ours to change
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := ApplyUnsubscribe(ctx, c.db, tenantID, p.Channel, p.Address)
	if err != nil {
		c.logger.Warn("unsubscribe event: update failed", zap.Error(err))
		return
	}
	c.logger.Info("contact opted out of marketing", zap.String("channel", p.Channel), zap.Int("contacts", n))
}

// ApplyUnsubscribe sets the matching contacts' consent flag false: email by its normalised form,
// sms and whatsapp by the phone's last nine digits (the platform's one-number key).
func ApplyUnsubscribe(ctx context.Context, db *ent.Client, tenantID uuid.UUID, channel, address string) (int, error) {
	switch channel {
	case "email":
		return db.Contact.Update().
			Where(contact.TenantID(tenantID), contact.EmailNormalized(strings.ToLower(strings.TrimSpace(address)))).
			SetSubscribedEmail(false).Save(ctx)
	case "sms", "whatsapp":
		digits := lastNineDigits(address)
		if len(digits) < 9 {
			return 0, nil
		}
		return db.Contact.Update().
			Where(contact.TenantID(tenantID), contact.PhoneSubscriberDigits(digits)).
			SetSubscribedSms(false).Save(ctx)
	}
	return 0, nil
}

func lastNineDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) > 9 {
		return d[len(d)-9:]
	}
	return d
}
