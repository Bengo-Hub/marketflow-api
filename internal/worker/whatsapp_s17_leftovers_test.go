package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
)

// The result of a WhatsApp publish is sent only once every post is finished, naming each one.
func TestPublishSummary(t *testing.T) {
	rows := []*ent.PublishJob{
		{Platform: publishing.Facebook, Status: publishing.StatusPublished, Permalink: "https://fb.example/p/1"},
		{Platform: publishing.Instagram, Status: publishing.StatusFailed, Error: "the media could not be fetched"},
	}
	summary, lines, done := publishSummary(rows)
	if !done || summary != "Facebook posted, Instagram failed" || len(lines) != 2 ||
		lines[0] != "Facebook: posted https://fb.example/p/1" || !strings.Contains(lines[1], "not posted (the media could not be fetched)") {
		t.Fatalf("summary %q lines %v done %v", summary, lines, done)
	}
	rows = append(rows, &ent.PublishJob{Platform: publishing.YouTube, Status: publishing.StatusPending})
	if _, _, done := publishSummary(rows); done {
		t.Fatal("a post still going must hold the message")
	}
	if _, _, done := publishSummary(nil); done {
		t.Fatal("no posts is nothing to report")
	}
	row := &ent.PublishJob{Metadata: map[string]any{publishing.MetaWhatsApp: map[string]any{"to": "+254700000001", "ref": "run-1:v1"}}}
	if to, ref := waRequester(row); to != "+254700000001" || ref != "run-1:v1" {
		t.Fatalf("requester: %q %q", to, ref)
	}
	if to, ref := waRequester(&ent.PublishJob{}); to != "" || ref != "" {
		t.Fatal("a Studio post has no WhatsApp requester")
	}
}

func TestBriefWhatsAppText(t *testing.T) {
	text := briefWhatsAppText("Jua Kali Salon", "Sunday 15 November", briefWords{Highlights: []string{"Leads up 40%", "Cost per lead down", "Third line"}})
	if !strings.Contains(text, "Jua Kali Salon (Sunday 15 November)") || !strings.Contains(text, "- Leads up 40%") ||
		strings.Contains(text, "Third line") || strings.Contains(text, "http") {
		t.Fatalf("text: %q", text)
	}
	// Without a Redis client nothing is remembered and nothing breaks.
	RememberAssistantNumber(context.Background(), nil, uuid.Nil, "u1", "+254700000001")
	if got := assistantRecipients(context.Background(), nil, nil, uuid.Nil, briefRoles); got != nil {
		t.Fatalf("recipients without redis: %v", got)
	}
	day := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	if (&DailyBrief{}).sendWhatsApp(context.Background(), &ent.Tenant{}, "2026-11-15", day, briefWords{}) != 0 {
		t.Fatal("no WhatsApp sender means email only")
	}
}

type fakeTopupMsg struct {
	fakeMsg
}

// Only credit purchases are passed to marketflow-ai; a 409 (credits not in yet) is retried; a
// payment that is not from WhatsApp sends nothing.
func TestWhatsAppTopupPaidHandler(t *testing.T) {
	status := http.StatusConflict
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/internal/os/whatsapp-topup-paid" || r.Header.Get("X-API-Key") != "ai-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["intent_id"] != "intent-1" || body["tenant_id"] != "t1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	h := NewWhatsAppTopupPaidHandler(srv.URL, "ai-key", nil, zap.NewNop())
	msg := func(refType string) fakeTopupMsg {
		b, _ := json.Marshal(map[string]any{"tenant_id": "t1", "intent_id": "intent-1", "reference_type": refType})
		return fakeTopupMsg{fakeMsg{data: b}}
	}
	if err := h.Handle(context.Background(), msg("subscription")); err != nil || calls != 0 {
		t.Fatalf("not a credit purchase: %v (%d calls)", err, calls)
	}
	if err := h.Handle(context.Background(), msg("credit_purchase")); !errors.Is(err, errCreditsNotLanded) {
		t.Fatalf("not landed: %v", err)
	}
	status = http.StatusOK
	if err := h.Handle(context.Background(), msg("credit_purchase")); err != nil || calls != 2 {
		t.Fatalf("landed, nothing to send: %v (%d calls)", err, calls)
	}
}
