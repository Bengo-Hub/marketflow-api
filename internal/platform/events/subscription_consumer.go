package events

import (
	"context"
	"encoding/json"
	"strings"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

const subscriptionTenantActivatedSubject = "subscription.tenant_activated"

// SubscriptionConsumer subscribes to subscription lifecycle events from subscriptions-api
// and upserts tenant admins as Contacts in MarketFlow for platform-level CRM campaigns.
type SubscriptionConsumer struct {
	conn   *nats.Conn
	orm    *ent.Client
	logger *zap.Logger
	subs   []*nats.Subscription
}

// NewSubscriptionConsumer creates a new consumer.
func NewSubscriptionConsumer(conn *nats.Conn, orm *ent.Client, logger *zap.Logger) *SubscriptionConsumer {
	return &SubscriptionConsumer{
		conn:   conn,
		orm:    orm,
		logger: logger.Named("subscription.consumer"),
	}
}

// Start subscribes to subscription events.
func (c *SubscriptionConsumer) Start() error {
	sub, err := eventslib.QueueSubscribe(c.logger, c.conn, subscriptionTenantActivatedSubject, "marketflow-subscription", c.handleTenantActivated)
	if err != nil {
		return err
	}
	c.subs = append(c.subs, sub)
	c.logger.Info("subscribed to subscription event", zap.String("subject", subscriptionTenantActivatedSubject))
	return nil
}

// Stop unsubscribes from all events.
func (c *SubscriptionConsumer) Stop() {
	for _, sub := range c.subs {
		_ = sub.Unsubscribe()
	}
}

type subscriptionTenantActivatedPayload struct {
	TenantID    string `json:"tenant_id"`
	TenantSlug  string `json:"tenant_slug"`
	PlanCode    string `json:"plan_code"`
	AdminUserID string `json:"admin_user_id"`
	AdminEmail  string `json:"admin_email"`
	AdminName   string `json:"admin_name"`
}

func (c *SubscriptionConsumer) handleTenantActivated(msg *nats.Msg) {
	var outer struct {
		Payload subscriptionTenantActivatedPayload `json:"payload"`
		// Also accept flat structure
		subscriptionTenantActivatedPayload
	}
	if err := json.Unmarshal(msg.Data, &outer); err != nil {
		c.logger.Warn("failed to parse subscription.tenant_activated event", zap.Error(err))
		return
	}

	// Prefer nested payload, fall back to flat
	p := outer.Payload
	if p.TenantID == "" {
		p = outer.subscriptionTenantActivatedPayload
	}
	if p.TenantID == "" || p.AdminEmail == "" {
		c.logger.Warn("subscription.tenant_activated missing tenant_id or admin_email — skipping CRM sync")
		return
	}

	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		c.logger.Warn("invalid tenant_id in subscription.tenant_activated", zap.String("tenant_id", p.TenantID))
		return
	}

	if err := c.upsertTenantAdminContact(tenantID, p); err != nil {
		c.logger.Warn("failed to upsert tenant admin contact",
			zap.String("tenant_id", p.TenantID),
			zap.String("email", p.AdminEmail),
			zap.Error(err),
		)
		return
	}

	c.logger.Info("tenant admin contact synced to MarketFlow",
		zap.String("tenant_slug", p.TenantSlug),
		zap.String("email", p.AdminEmail),
	)
}

func (c *SubscriptionConsumer) upsertTenantAdminContact(tenantID uuid.UUID, p subscriptionTenantActivatedPayload) error {
	ctx := context.Background()

	existing, err := c.orm.Contact.Query().
		Where(contact.EmailEQ(p.AdminEmail)).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return err
	}

	firstName, lastName := splitName(p.AdminName)
	enrichment := map[string]any{
		"tenant_slug":       p.TenantSlug,
		"subscription_plan": p.PlanCode,
		"source":            "subscription_activation",
	}

	if ent.IsNotFound(err) {
		// Create new contact
		_, err = c.orm.Contact.Create().
			SetTenantID(tenantID).
			SetEmail(p.AdminEmail).
			SetNillableFirstName(&firstName).
			SetNillableLastName(nullStr(lastName)).
			SetContactType("platform_tenant").
			SetEnrichmentData(enrichment).
			Save(ctx)
		return err
	}

	// Update existing contact with latest subscription info
	_, err = existing.Update().
		SetNillableFirstName(&firstName).
		SetNillableLastName(nullStr(lastName)).
		SetEnrichmentData(enrichment).
		Save(ctx)
	return err
}

func splitName(name string) (first, last string) {
	parts := strings.SplitN(strings.TrimSpace(name), " ", 2)
	if len(parts) == 0 {
		return "", ""
	}
	first = parts[0]
	if len(parts) > 1 {
		last = parts[1]
	}
	return
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
