// Package notifications wraps the platform notifications-api so marketflow
// never talks to Brevo/Twilio/Meta directly. Secrets stay in notifications-api's
// platform ProviderSetting store.
package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
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

// Message is one notification for one recipient. Send maps it onto notifications-api's
// request (see sendRequest).
//
// Either name a template (TemplateID, channel-relative, e.g. "shared/generic_notification",
// with its variables in Data) or give the text in Subject and Body. A Body is sent through the
// platform's "broadcast/message" template: email renders it as paragraphs (split on blank
// lines), SMS and WhatsApp as the text itself. HTML is not supported (notifications-api escapes
// it); tags are stripped so the text still goes out.
type Message struct {
	Channel        string
	TemplateID     string
	Recipient      string
	Data           map[string]any
	Subject        string
	Body           string
	Metadata       map[string]string
	IdempotencyKey string // sent as the Idempotency-Key header
}

// bodyTemplate renders a ready-made Body (email: data.paragraphs; sms, whatsapp: data.body).
const bodyTemplate = "broadcast/message"

// sendRequest is notifications-api's POST /api/v1/{tenantId}/notifications/messages body
// (CreateMessageRequest). It decodes without strict fields, so a wrong name is dropped
// silently: the old template_id and recipient names left template and to empty, and every
// email failed with "no valid recipient addresses".
type sendRequest struct {
	Channel  string         `json:"channel"`
	Tenant   string         `json:"tenant"`
	Template string         `json:"template"`
	Data     map[string]any `json:"data"`
	To       []string       `json:"to"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// toRequest builds the wire request for tenantID.
func toRequest(tenantID string, msg Message) (sendRequest, error) {
	if strings.TrimSpace(msg.Recipient) == "" {
		return sendRequest{}, fmt.Errorf("notifications: recipient required")
	}
	req := sendRequest{
		Channel:  msg.Channel,
		Tenant:   tenantID,
		Template: msg.TemplateID,
		Data:     map[string]any{},
		To:       []string{strings.TrimSpace(msg.Recipient)},
		Metadata: map[string]any{},
	}
	for k, v := range msg.Data {
		req.Data[k] = v
	}
	for k, v := range msg.Metadata {
		req.Metadata[k] = v
	}
	if msg.Subject != "" {
		req.Metadata["subject"] = msg.Subject
		if msg.Channel == ChannelPush {
			req.Metadata["push_title"] = msg.Subject
		}
	}
	if msg.Body != "" {
		text := plainText(msg.Body)
		req.Template = bodyTemplate
		req.Data["body"] = text
		req.Data["paragraphs"] = paragraphs(text)
	}
	if req.Template == "" {
		return sendRequest{}, fmt.Errorf("notifications: a template or a body is required")
	}
	return req, nil
}

var (
	blockTagRe = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/tr|/h[1-6]|/li)\s*/?>`)
	anyTagRe   = regexp.MustCompile(`<[^>]*>`)
	blankRunRe = regexp.MustCompile(`\n{3,}`)
	paraRe     = regexp.MustCompile(`\n\s*\n`)
)

// plainText turns an HTML body into text (block ends become line breaks); plain text is kept.
func plainText(s string) string {
	if !strings.Contains(s, "<") {
		return strings.TrimSpace(s)
	}
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.TrimSpace(blankRunRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// paragraphs splits text on blank lines; single line breaks inside a paragraph become spaces.
func paragraphs(text string) []string {
	var out []string
	for _, p := range paraRe.Split(text, -1) {
		if p = strings.Join(strings.Fields(p), " "); p != "" {
			out = append(out, p)
		}
	}
	return out
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
	wire, err := toRequest(tenantID, msg)
	if err != nil {
		return err
	}
	body, err := json.Marshal(wire)
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

// BroadcastRequest hands a bulk send to notifications-api's broadcast engine, which pages the
// audience, applies consent and opt-outs, paces the sends and waits for the tenant's approval.
type BroadcastRequest struct {
	TenantID    string         `json:"tenant_id"`
	RequestedBy string         `json:"requested_by,omitempty"`
	Source      string         `json:"source"`
	SourceRef   string         `json:"source_ref,omitempty"`
	Submit      bool           `json:"submit"`
	Broadcast   map[string]any `json:"broadcast"`
}

// BroadcastResult is the created broadcast's id and status.
type BroadcastResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// CreateBroadcast calls POST /api/v1/s2s/broadcasts (internal key).
func (c *Client) CreateBroadcast(ctx context.Context, req BroadcastRequest) (*BroadcastResult, error) {
	if c.baseURL == "" || c.apiKey == "" {
		return nil, fmt.Errorf("notifications: base URL or service key not configured")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/s2s/broadcasts", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("User-Agent", c.userAgent)
	hr.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("notifications broadcast: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("notifications %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out BroadcastResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
