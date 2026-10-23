package worker

import (
	"context"
	"encoding/json"
	"fmt"
	sharedcache "github.com/Bengo-Hub/cache"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/notifications"
)

// SLABreachCron queries ticketing-api every 5 minutes to detect SLA breaches and
// marks them via S2S PATCH. Runs as a goroutine started from cmd/worker/main.go.
type SLABreachCron struct {
	ticketingURL string
	notifURL     string
	notif        *notifications.Client
	serviceKey   string
	logger       *zap.Logger
	hc           *http.Client
	interval     time.Duration
}

// NewSLABreachCron constructs an SLABreachCron with a 5-minute default interval.
func NewSLABreachCron(ticketingURL, notifURL, serviceKey string, logger *zap.Logger, hc *http.Client) *SLABreachCron {
	return &SLABreachCron{
		ticketingURL: ticketingURL,
		notifURL:     notifURL,
		notif:        notifications.New(notifications.Config{BaseURL: notifURL, APIKey: serviceKey, Timeout: 5 * time.Second}),
		serviceKey:   serviceKey,
		logger:       logger.Named("sla-breach-cron"),
		hc:           hc,
		interval:     5 * time.Minute,
	}
}

// Run blocks until ctx is cancelled, running SLA breach detection on each tick.
func (c *SLABreachCron) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	c.logger.Info("SLA breach cron started", zap.Duration("interval", c.interval))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

func (c *SLABreachCron) runOnce(ctx context.Context) {
	// Runs on every replica's ticker; only the first replica in each period does the work.
	if !sharedcache.ClaimPeriod(ctx, "marketflow:sla-breach", c.interval) {
		return
	}
	// Query ticketing-api for response SLA breaches.
	c.checkBreaches(ctx, "response")
	// Query ticketing-api for resolution SLA breaches.
	c.checkBreaches(ctx, "resolution")
}

type slaBreachedTicket struct {
	ID              string  `json:"id"`
	TenantID        string  `json:"tenant_id"`
	TicketNumber    string  `json:"ticket_number"`
	Subject         string  `json:"subject"`
	AssignedAgentID *string `json:"assigned_agent_id"`
}

func (c *SLABreachCron) checkBreaches(ctx context.Context, breachType string) {
	url := fmt.Sprintf("%s/api/v1/internal/tickets?sla_breach_check=%s", c.ticketingURL, breachType)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.logger.Error("sla breach: build request failed", zap.String("type", breachType), zap.Error(err))
		return
	}
	req.Header.Set("X-API-Key", c.serviceKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		c.logger.Warn("sla breach: query failed", zap.String("type", breachType), zap.Error(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return
	}
	if resp.StatusCode >= 300 {
		c.logger.Warn("sla breach: unexpected status",
			zap.String("type", breachType),
			zap.Int("status", resp.StatusCode),
		)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	var result struct {
		Data []slaBreachedTicket `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		c.logger.Error("sla breach: unmarshal failed", zap.Error(err))
		return
	}

	for _, ticket := range result.Data {
		c.markBreached(ctx, ticket, breachType)
	}
}

func (c *SLABreachCron) markBreached(ctx context.Context, ticket slaBreachedTicket, breachType string) {
	fieldName := "sla_response_breached"
	if breachType == "resolution" {
		fieldName = "sla_resolution_breached"
	}

	patchURL := fmt.Sprintf("%s/api/v1/internal/tickets/%s/sla-breach", c.ticketingURL, ticket.ID)
	payload := map[string]any{fieldName: true}

	if err := agentS2SPost(ctx, c.hc, patchURL, c.serviceKey, payload); err != nil {
		c.logger.Error("sla breach: mark failed",
			zap.String("ticket_id", ticket.ID),
			zap.String("type", breachType),
			zap.Error(err),
		)
		return
	}

	c.logger.Info("sla breach marked",
		zap.String("ticket_id", ticket.ID),
		zap.String("ticket_number", ticket.TicketNumber),
		zap.String("type", breachType),
	)

	// Notify assigned agent via notifications-api.
	if ticket.AssignedAgentID != nil && c.notifURL != "" {
		c.notifyBreach(ctx, ticket, breachType)
	}
}

func (c *SLABreachCron) notifyBreach(ctx context.Context, ticket slaBreachedTicket, breachType string) {
	notifCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	subject := fmt.Sprintf("SLA %s breach: Ticket #%s", breachType, ticket.TicketNumber)
	body := fmt.Sprintf("Ticket #%s (%s) has breached its %s SLA.", ticket.TicketNumber, ticket.Subject, breachType)

	// Through the shared client: the old POST /api/v1/notifications/send is not a
	// notifications-api route, so every breach alert answered 404.
	err := c.notif.Send(notifCtx, ticket.TenantID, notifications.Message{
		Channel:    notifications.ChannelPush,
		TemplateID: "generic",
		Recipient:  *ticket.AssignedAgentID,
		Subject:    subject,
		Data:       map[string]any{"title": subject, "body": body},
		Metadata:   map[string]string{"entity_type": "ticket", "entity_id": ticket.ID, "source": "marketflow-worker"},
	})
	if err != nil {
		c.logger.Warn("sla breach notification failed",
			zap.String("ticket_id", ticket.ID),
			zap.Error(err),
		)
	}
}
