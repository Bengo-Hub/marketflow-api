package events

import (
	"context"
	"encoding/json"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// LoyaltyConsumer subscribes to pos.loyalty.tier_upgraded and writes the new tier
// into the CRM contact's metadata so the 360° view reflects current loyalty status.
type LoyaltyConsumer struct {
	db     *ent.Client
	logger *zap.Logger
	sub    *nats.Subscription
}

// NewLoyaltyConsumer creates a new consumer.
func NewLoyaltyConsumer(db *ent.Client, logger *zap.Logger) *LoyaltyConsumer {
	return &LoyaltyConsumer{
		db:     db,
		logger: logger.Named("loyalty.consumer"),
	}
}

// Start subscribes to the loyalty tier upgrade event.
func (c *LoyaltyConsumer) Start(conn *nats.Conn) error {
	sub, err := eventslib.QueueSubscribe(c.logger, conn, "pos.loyalty.tier_upgraded", "marketflow-loyalty", c.handle)
	if err != nil {
		return err
	}
	c.sub = sub
	c.logger.Info("subscribed to pos.loyalty.tier_upgraded")
	return nil
}

// Stop drains the subscription.
func (c *LoyaltyConsumer) Stop() {
	if c.sub != nil {
		_ = c.sub.Drain()
	}
}

type loyaltyTierUpgradedPayload struct {
	AccountID     string `json:"account_id"`
	CustomerPhone string `json:"customer_phone"`
	CustomerName  string `json:"customer_name"`
	CRMContactID  string `json:"crm_contact_id"`
	OldTier       string `json:"old_tier"`
	NewTier       string `json:"new_tier"`
	LifetimePoints int   `json:"lifetime_points"`
	ProgramID     string `json:"program_id"`
}

func (c *LoyaltyConsumer) handle(msg *nats.Msg) {
	var wrapper struct {
		TenantID string          `json:"tenant_id"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &wrapper); err != nil {
		c.logger.Warn("loyalty.tier_upgraded: bad envelope", zap.Error(err))
		return
	}

	var p loyaltyTierUpgradedPayload
	if err := json.Unmarshal(wrapper.Payload, &p); err != nil {
		c.logger.Warn("loyalty.tier_upgraded: bad payload", zap.Error(err))
		return
	}

	tenantID, err := uuid.Parse(wrapper.TenantID)
	if err != nil {
		c.logger.Warn("loyalty.tier_upgraded: invalid tenant_id", zap.Error(err))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// If the event carries a direct CRM contact ID, use it; otherwise look up by phone.
	var contactID uuid.UUID
	if p.CRMContactID != "" {
		contactID, err = uuid.Parse(p.CRMContactID)
		if err != nil {
			contactID = uuid.Nil
		}
	}

	var existing *ent.Contact
	if contactID != uuid.Nil {
		existing, err = c.db.Contact.Query().
			Where(contact.ID(contactID), contact.TenantID(tenantID)).
			First(ctx)
	}
	if existing == nil && p.CustomerPhone != "" {
		existing, err = c.db.Contact.Query().
			Where(contact.TenantID(tenantID), contact.PhoneEQ(p.CustomerPhone)).
			First(ctx)
	}
	if existing == nil {
		c.logger.Debug("loyalty.tier_upgraded: no CRM contact found, skipping metadata update",
			zap.String("phone", p.CustomerPhone))
		return
	}

	// Merge loyalty tier metadata — preserve any existing metadata fields.
	meta := make(map[string]any)
	if existing.Metadata != nil {
		for k, v := range existing.Metadata {
			meta[k] = v
		}
	}
	meta["loyalty_tier"] = p.NewTier
	meta["loyalty_lifetime_points"] = p.LifetimePoints
	meta["loyalty_tier_updated_at"] = time.Now().UTC().Format(time.RFC3339)

	_, err = existing.Update().SetMetadata(meta).Save(ctx)
	if err != nil {
		c.logger.Error("loyalty.tier_upgraded: failed to update contact metadata",
			zap.String("contact_id", existing.ID.String()),
			zap.Error(err),
		)
		return
	}

	c.logger.Info("loyalty tier updated on CRM contact",
		zap.String("contact_id", existing.ID.String()),
		zap.String("old_tier", p.OldTier),
		zap.String("new_tier", p.NewTier),
	)
}
