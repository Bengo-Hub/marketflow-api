package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowrole"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/bengobox/marketflow-api/internal/ent/userroleassignment"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
)

// The alert carries everything needed to answer the lead (owner, 2026-11-24): name, interest,
// message, phone, a WhatsApp click-to-chat link, email, source and the lead link.
func TestLeadAlertTextNamesEveryDetail(t *testing.T) {
	one := leadAlertItem{Name: "Esther Masika", Interest: "Enterprise Software Development", Message: "We need an ERP.",
		Phone: "0712 345 678", WhatsApp: whatsAppChatLink("0712 345 678", "KE"), Email: "esther@example.com",
		Source: sourceLabel("website_contact"), Link: "https://p/codevertex/leads/1"}
	subject, body := leadAlertText([]leadAlertItem{one}, 15*time.Minute)
	if subject != "New lead: Esther Masika" {
		t.Fatalf("subject %q", subject)
	}
	for _, want := range []string{"Interested in: Enterprise Software Development", "Message: We need an ERP.", "Phone: 0712 345 678",
		"WhatsApp: https://wa.me/254712345678", "Email: esther@example.com", "Came in from: your website contact form",
		"Open the lead: https://p/codevertex/leads/1", "15 minutes"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	two := one
	two.Name = "Brian"
	subject, body = leadAlertText([]leadAlertItem{one, two, two}, 15*time.Minute)
	if subject != "3 new leads: Esther Masika and 2 more" || strings.Count(body, "Open the lead:") != 3 {
		t.Fatalf("burst email must list every lead: %q\n%s", subject, body)
	}
	if whatsAppChatLink("12", "") != "" || whatsAppChatLink("+44 7700 900123", "GB") != "https://wa.me/447700900123" {
		t.Fatal("click-to-chat link")
	}
}

func TestRedisLeadAlertClaimWithoutRedis(t *testing.T) {
	if !redisLeadAlertClaim(nil)(t.Context(), [16]byte{}, time.Minute) {
		t.Fatal("without Redis every lead alerts")
	}
}

// seedTenantAdmin gives a test tenant one marketflow_admin with an email.
func seedTenantAdmin(t *testing.T, db *ent.Client, tid uuid.UUID, email string) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = db.UserRoleAssignment.Delete().Where(userroleassignment.TenantID(tid)).Exec(ctx)
		_, _ = db.MarketflowRole.Delete().Where(marketflowrole.TenantID(tid)).Exec(ctx)
		_, _ = db.MarketflowUser.Delete().Where(marketflowuser.TenantID(tid)).Exec(ctx)
	})
	role, err := db.MarketflowRole.Create().SetTenantID(tid).SetRoleCode("marketflow_admin").SetName("Admin").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u, err := db.MarketflowUser.Create().SetTenantID(tid).SetAuthServiceUserID(uuid.New()).SetEmail(email).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserRoleAssignment.Create().SetTenantID(tid).SetUserID(u.ID).SetRoleID(role.ID).SetAssignedBy(u.ID).Save(ctx); err != nil {
		t.Fatal(err)
	}
}

// captureNotifications is a notifications-api stand-in that records each send.
func captureNotifications(t *testing.T) (*notifications.Client, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var sent []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		sent = append(sent, m)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return notifications.New(notifications.Config{BaseURL: srv.URL}), func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), sent...)
	}
}

// A burst is never silent: the first lead alerts at once, the rest wait for the window and go
// out together in one email listing them all, and no lead is alerted twice.
func TestLeadAlertBatchesABurst(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	if _, err := leadresponse.EnsureStartedAt(ctx, db, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	tid := seedTenant(t, db, "alert")
	seedTenantAdmin(t, db, tid, "owner@example.com")
	notif, sent := captureNotifications(t)
	h := NewNewLeadAlertHandler(db, nil, notif, "https://portal", zap.NewNop())
	windowOpen := false
	h.claim = func(context.Context, uuid.UUID, time.Duration) bool {
		if windowOpen {
			return false
		}
		windowOpen = true
		return true
	}
	mk := func(name, msg string) *ent.Lead {
		l, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetFirstName(name).
			SetPhone("0712000111").SetMetadata(map[string]any{"message": msg}).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	tn, _ := db.Tenant.Get(ctx, tid)
	first := mk("First", "hello")
	if n, err := h.alert(ctx, tn, first.ID); err != nil || n != 1 {
		t.Fatalf("first lead: n=%d err=%v", n, err)
	}
	second, third := mk("Second", "two"), mk("Third", "three")
	for _, l := range []*ent.Lead{second, third} {
		if n, _ := h.alert(ctx, tn, l.ID); n != 0 {
			t.Fatal("inside the window a lead waits")
		}
	}
	if got := len(sent()); got != 1 {
		t.Fatalf("sent %d emails, want 1 so far", got)
	}
	windowOpen = false // the window ends
	if n, err := h.Flush(ctx, tn); err != nil || n != 2 {
		t.Fatalf("flush: n=%d err=%v", n, err)
	}
	msgs := sent()
	if len(msgs) != 2 {
		t.Fatalf("sent %d emails, want 2", len(msgs))
	}
	body := dataBody(msgs[1])
	if !strings.Contains(body, "Second") || !strings.Contains(body, "Third") || strings.Contains(body, "First") {
		t.Fatalf("the batched email lists the waiting leads only:\n%s", body)
	}
	windowOpen = false
	if n, _ := h.Flush(ctx, tn); n != 0 || len(sent()) != 2 {
		t.Fatal("nothing waits, so nothing more is sent")
	}
}
