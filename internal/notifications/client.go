// Package notifications wraps the platform notifications-api so marketflow
// never talks to Brevo/Twilio/Meta directly. Secrets stay in notifications-api's
// platform ProviderSetting store.
package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Channel values recognised by notifications-api.
const (
	ChannelEmail    = "email"
	ChannelSMS      = "sms"
	ChannelWhatsApp = "whatsapp"
	ChannelPush     = "push"
)

// Message is the payload POSTed to /api/v1/{tenantId}/notifications/messages.
type Message struct {
	Channel        string            `json:"channel"`
	TemplateID     string            `json:"template_id,omitempty"`
	Recipient      string            `json:"recipient"`
	Data           map[string]any    `json:"data,omitempty"`
	Subject        string            `json:"subject,omitempty"`
	Body           string            `json:"body,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	IdempotencyKey string            `json:"-"` // sent as header
}

// Client is a thin HTTP client for notifications-api.
type Client struct {
	baseURL   string
	http      *http.Client
	authToken string
	apiKey    string
	userAgent string
}

// Config configures the Client.
type Config struct {
	BaseURL   string
	AuthToken string
	APIKey    string
	Timeout   time.Duration
}

// New constructs a notifications client.
func New(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Client{
		baseURL:   strings.TrimSuffix(cfg.BaseURL, "/"),
		http:      &http.Client{Timeout: cfg.Timeout},
		authToken: cfg.AuthToken,
		apiKey:    cfg.APIKey,
		userAgent: "marketflow-worker/1.0",
	}
}

// Send enqueues a message for delivery.
func (c *Client) Send(ctx context.Context, tenantID string, msg Message) error {
	if c.baseURL == "" {
		return fmt.Errorf("notifications: base URL not configured")
	}
	if tenantID == "" {
		return fmt.Errorf("notifications: tenantID required")
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	url := fmt.Sprintf("%s/api/v1/%s/notifications/messages", c.baseURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-Tenant-ID", tenantID)
	if msg.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", msg.IdempotencyKey)
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("notifications send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("notifications %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
