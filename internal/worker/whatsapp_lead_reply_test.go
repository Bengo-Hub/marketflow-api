package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// Sprint 23 L25/L26: a lead who replies on WhatsApp after the instant follow-up gets an answer
// from the business's assistant; asking for a person hands the lead to the owner, who is emailed,
// and the bot stops answering that lead.
func TestHandleLeadReply(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "leadreply")
	if err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{"sales_handoff_value": 100000.0}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	admin, _ := db.MarketflowRole.Create().SetTenantID(tid).SetRoleCode("marketflow_admin").SetName("Admin").Save(ctx)
	u, err := db.MarketflowUser.Create().SetTenantID(tid).SetAuthServiceUserID(uuid.New()).SetEmail("owner@example.com").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserRoleAssignment.Create().SetTenantID(tid).SetUserID(u.ID).SetRoleID(admin.ID).SetAssignedBy(u.ID).Save(ctx); err != nil {
		t.Fatal(err)
	}

	// A unique 9-digit suffix per run so other runs' leads never match.
	suffix := fmt.Sprintf("7%08d", time.Now().UnixNano()%100000000)
	phone := "+254" + suffix
	firstReply := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	l, err := db.Lead.Create().SetTenantID(tid).SetSource("meta_ad").SetLeadType("customer").SetPhone(phone).
		SetFirstName("Wanjiru").SetMetadata(map[string]any{"first_reply_at": firstReply}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Not enrolled yet: the message is not treated as a lead's.
	h := &WhatsAppAssistantHandler{db: db, http: http.DefaultClient, logger: zap.NewNop(), portalURL: "https://portal.example.com"}
	if ok, err := h.handleLeadReply(ctx, waInbound{From: "254" + suffix, WaMessageID: "m0", Text: "hi"}); ok || err != nil {
		t.Fatalf("a lead outside automated follow-up is left alone: %v %v", ok, err)
	}
	seq, err := db.NurtureSequence.Create().SetTenantID(tid).SetName("Welcome").SetTriggerEvent("lead.created").
		SetActive(true).SetSteps([]map[string]any{}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NurtureEnrollment.Create().SetTenantID(tid).SetSequenceID(seq.ID).SetLeadID(l.ID).Save(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var asked []map[string]any
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/internal/os/lead-reply" || r.Header.Get("X-API-Key") != "ai-key" {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		asked = append(asked, m)
		mu.Unlock()
		if strings.Contains(m["text"].(string), "person") {
			_ = json.NewEncoder(w).Encode(map[string]any{"reply": "I have asked the team.", "handoff": true, "handoff_reason": "asked for a person"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"reply": "The course is KES 5,000."})
	}))
	defer ai.Close()
	var sent []map[string]any
	notif := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		if strings.Contains(r.URL.Path, tid.String()) {
			sent = append(sent, m)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"status":"queued"}`)
	}))
	defer notif.Close()
	h.aiURL, h.aiKey = ai.URL, "ai-key"
	h.notif = notifications.New(notifications.Config{BaseURL: notif.URL, APIKey: "test"})

	if ok, err := h.handleLeadReply(ctx, waInbound{From: "254" + suffix, WaMessageID: "m1", Text: "How much is it?"}); !ok || err != nil {
		t.Fatalf("lead reply: %v %v", ok, err)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0]["lead_id"] != l.ID.String() || asked[0]["handoff_value"] != 100000.0 || asked[0]["business_type"] != "flp" {
		t.Fatalf("the assistant is asked with the lead and hand-off value: %+v", asked)
	}
	if len(sent) != 1 || firstTo(sent[0]) != phone || !strings.Contains(dataBody(sent[0]), "5,000") {
		t.Fatalf("the answer goes to the lead on WhatsApp from the tenant: %+v", sent)
	}
	mu.Unlock()
	n, _ := db.Activity.Query().Where(activity.EntityID(l.ID), activity.ActivityType("whatsapp")).Count(ctx)
	if n != 2 {
		t.Errorf("both messages are on the lead's timeline, got %d", n)
	}

	if _, err := h.handleLeadReply(ctx, waInbound{From: "254" + suffix, WaMessageID: "m2", Text: "can I talk to a person"}); err != nil {
		t.Fatal(err)
	}
	got, _ := db.Lead.Get(ctx, l.ID)
	if got.Metadata["sales_handoff_at"] == nil || got.Metadata["sales_handoff_reason"] != "asked for a person" {
		t.Fatalf("hand-off recorded on the lead: %+v", got.Metadata)
	}
	mu.Lock()
	var emailed bool
	for _, m := range sent {
		if firstTo(m) == "owner@example.com" && strings.Contains(dataBody(m), "talk to a person") {
			emailed = true
		}
	}
	mu.Unlock()
	if !emailed {
		t.Errorf("the owner is emailed with the lead's message: %+v", sent)
	}

	// After the hand-off the bot stays quiet for this lead.
	if ok, _ := h.handleLeadReply(ctx, waInbound{From: "254" + suffix, WaMessageID: "m3", Text: "hello?"}); ok {
		t.Error("a handed-off lead is not answered by the bot")
	}
	mu.Lock()
	if len(asked) != 2 {
		t.Errorf("no AI call after the hand-off, got %d", len(asked))
	}
	mu.Unlock()
	_ = ent.Lead{}
}

// Sprint 23 L45: with the assistant switched off, a lead's message is kept on the timeline and
// no AI answer is sent.
func TestHandleLeadReplyOffSwitch(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "leadoff")
	if err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{"sales_auto_reply": false}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("8%08d", time.Now().UnixNano()%100000000)
	l, err := db.Lead.Create().SetTenantID(tid).SetSource("meta_ad").SetLeadType("customer").SetPhone("+254" + suffix).
		SetMetadata(map[string]any{"first_reply_at": time.Now().UTC().Format(time.RFC3339)}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seq, _ := db.NurtureSequence.Create().SetTenantID(tid).SetName("Welcome").SetTriggerEvent("lead.created").
		SetActive(true).SetSteps([]map[string]any{}).Save(ctx)
	if _, err := db.NurtureEnrollment.Create().SetTenantID(tid).SetSequenceID(seq.ID).SetLeadID(l.ID).Save(ctx); err != nil {
		t.Fatal(err)
	}
	called := false
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer ai.Close()
	h := &WhatsAppAssistantHandler{db: db, http: http.DefaultClient, logger: zap.NewNop(), aiURL: ai.URL}
	if ok, err := h.handleLeadReply(ctx, waInbound{From: "254" + suffix, WaMessageID: "off1", Text: "price?"}); !ok || err != nil {
		t.Fatalf("still a lead's message: %v %v", ok, err)
	}
	if called {
		t.Error("no AI answer when the assistant is off")
	}
	if n, _ := db.Activity.Query().Where(activity.EntityID(l.ID)).Count(ctx); n != 1 {
		t.Errorf("the message is on the timeline, got %d", n)
	}
}
