package events

import (
	"context"
	"encoding/json"

	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// TenantConsumer subscribes to tenant lifecycle events from auth-api.
type TenantConsumer struct {
	conn   *nats.Conn
	syncer *tenant.Syncer
	logger *zap.Logger
	subs   []*nats.Subscription
}

// NewTenantConsumer creates a new consumer.
func NewTenantConsumer(conn *nats.Conn, syncer *tenant.Syncer, logger *zap.Logger) *TenantConsumer {
	return &TenantConsumer{
		conn:   conn,
		syncer: syncer,
		logger: logger.Named("tenant.consumer"),
	}
}

// Start subscribes to tenant events.
func (c *TenantConsumer) Start() error {
	subjects := []string{"auth.tenant.created", "auth.tenant.updated"}
	for _, subject := range subjects {
		sub, err := c.conn.Subscribe(subject, c.handleTenantEvent)
		if err != nil {
			return err
		}
		c.subs = append(c.subs, sub)
		c.logger.Info("subscribed to tenant event", zap.String("subject", subject))
	}
	return nil
}

// Stop unsubscribes from all events.
func (c *TenantConsumer) Stop() {
	for _, sub := range c.subs {
		_ = sub.Unsubscribe()
	}
}

func (c *TenantConsumer) handleTenantEvent(msg *nats.Msg) {
	var event struct {
		Payload struct {
			Slug string `json:"slug"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		c.logger.Warn("failed to parse tenant event", zap.Error(err))
		return
	}

	slug := event.Payload.Slug
	if slug == "" {
		c.logger.Warn("tenant event missing slug")
		return
	}

	_, err := c.syncer.SyncTenant(context.Background(), slug)
	if err != nil {
		c.logger.Warn("tenant sync from event failed",
			zap.String("slug", slug),
			zap.Error(err),
		)
		return
	}

	c.logger.Info("tenant synced from NATS event", zap.String("slug", slug))
}
