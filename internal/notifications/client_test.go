package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// createMessageRequest mirrors notifications-api's CreateMessageRequest
// (internal/http/handlers/notification.go), the shape its Enqueue handler decodes. A field
// sent under another name is dropped there silently, so the test decodes exactly that shape.
type createMessageRequest struct {
	Channel  string         `json:"channel"`
	Tenant   string         `json:"tenant"`
	Template string         `json:"template"`
	Data     map[string]any `json:"data"`
	To       []string       `json:"to"`
	Metadata map[string]any `json:"metadata"`
}

func capture(t *testing.T) (*Client, *createMessageRequest, *http.Header, *string) {
	t.Helper()
	var got createMessageRequest
	var hdr http.Header
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = createMessageRequest{}
		hdr, path = r.Header.Clone(), r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, APIKey: "k"}), &got, &hdr, &path
}

func TestSendBodyUsesBroadcastTemplate(t *testing.T) {
	c, got, hdr, path := capture(t)
	err := c.Send(context.Background(), "t1", Message{
		Channel: ChannelEmail, Recipient: "owner@example.com", Subject: "Reconnect Meta",
		Body:           "MarketFlow can no longer post.\n\n- Shop (facebook page) has stopped working\n\nReconnect here: https://x",
		IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if *path != "/api/v1/t1/notifications/messages" || hdr.Get("Idempotency-Key") != "k1" || hdr.Get("X-API-Key") != "k" {
		t.Errorf("request: %s %v", *path, hdr)
	}
	if got.Tenant != "t1" || got.Template != "broadcast/message" || len(got.To) != 1 || got.To[0] != "owner@example.com" {
		t.Fatalf("contract: %+v", got)
	}
	if got.Metadata["subject"] != "Reconnect Meta" {
		t.Errorf("subject goes in metadata: %v", got.Metadata)
	}
	paras, _ := got.Data["paragraphs"].([]any)
	if len(paras) != 3 || paras[2] != "Reconnect here: https://x" || !strings.Contains(got.Data["body"].(string), "stopped working") {
		t.Errorf("body as paragraphs and text: %v", got.Data)
	}
}

func TestSendHTMLBodyBecomesText(t *testing.T) {
	c, got, _, _ := capture(t)
	if err := c.Send(context.Background(), "t1", Message{
		Channel: ChannelEmail, Recipient: "a@example.com", Subject: "Brief",
		Body: `<div><h2>Good morning, Acme &amp; Co</h2><table><tr><td>Leads</td><td><b>12</b></td></tr></table><p>Next</p></div>`,
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := got.Data["body"].(string)
	if strings.Contains(body, "<") || !strings.Contains(body, "Good morning, Acme & Co") || !strings.Contains(body, "Leads 12") {
		t.Errorf("HTML is turned into readable text: %q", body)
	}
}

func TestSendNamedTemplateAndValidation(t *testing.T) {
	c, got, _, _ := capture(t)
	if err := c.Send(context.Background(), "t1", Message{
		Channel: ChannelPush, TemplateID: "generic", Recipient: "user-1", Subject: "SLA breach",
		Data: map[string]any{"title": "SLA breach", "body": "Ticket #7"},
	}); err != nil {
		t.Fatal(err)
	}
	if got.Template != "generic" || got.Data["body"] != "Ticket #7" || got.Metadata["push_title"] != "SLA breach" {
		t.Errorf("named template keeps its data: %+v", got)
	}
	if err := c.Send(context.Background(), "t1", Message{Channel: ChannelEmail, Recipient: "a@example.com"}); err == nil {
		t.Error("a message without a template or body is refused before sending")
	}
	if err := c.Send(context.Background(), "t1", Message{Channel: ChannelEmail, Body: "x"}); err == nil {
		t.Error("a message without a recipient is refused before sending")
	}
}
