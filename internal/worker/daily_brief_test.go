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

	"github.com/bengobox/marketflow-api/internal/ent/marketflowrole"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/bengobox/marketflow-api/internal/ent/userroleassignment"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

func TestRenderBriefEscapesAndFormats(t *testing.T) {
	body := renderBrief("Amani <Beauty>", []marketingos.BriefMetric{
		{Key: "orders", Label: "Orders from CRM contacts", Value: 4, Baseline: 2},
		{Key: "ad_spend", Label: "Ad spend", Value: 12500, Baseline: 12500, Unit: "KES"},
		{Key: "leads", Label: "New leads", Value: 0, Baseline: 0},
	}, briefWords{Insights: []string{"Orders doubled <script>"}, Actions: []string{"Reply to comments"}}, "https://x/t/dashboard")
	for _, want := range []string{"Amani &lt;Beauty&gt;", "KES 12,500", "up 100%", "flat", "no change", "Orders doubled &lt;script&gt;", "Reply to comments"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("model text must be escaped")
	}
	if commas("1234567") != "1,234,567" || commas("-1000") != "-1,000" || commas("999") != "999" {
		t.Fatal("commas")
	}
}

func TestDailyBriefSendsOncePerLocalDay(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "brief")
	t.Cleanup(func() {
		_, _ = db.UserRoleAssignment.Delete().Where(userroleassignment.TenantID(tid)).Exec(ctx)
		_, _ = db.MarketflowRole.Delete().Where(marketflowrole.TenantID(tid)).Exec(ctx)
		_, _ = db.MarketflowUser.Delete().Where(marketflowuser.TenantID(tid)).Exec(ctx)
	})
	tz := "Africa/Nairobi"
	if _, err := db.Tenant.UpdateOneID(tid).SetTimezone(tz).Save(ctx); err != nil {
		t.Fatal(err)
	}
	role, _ := db.MarketflowRole.Create().SetTenantID(tid).SetRoleCode("marketflow_admin").SetName("Admin").Save(ctx)
	u, err := db.MarketflowUser.Create().SetTenantID(tid).SetAuthServiceUserID(uuid.New()).SetEmail("owner@example.com").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserRoleAssignment.Create().SetTenantID(tid).SetUserID(u.ID).SetRoleID(role.ID).SetAssignedBy(u.ID).Save(ctx); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation(tz)
	now := time.Date(2026, 10, 22, 7, 0, 0, 0, loc)
	if _, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetStatus("new").
		SetFirstName("A").SetCreatedAt(now.AddDate(0, 0, -1)).Save(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var sent []map[string]any
	notif := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		sent = append(sent, m)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer notif.Close()
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"highlights":["leads"],"insights":["One new lead came in."],"actions":["Call the new lead today."]}`))
	}))
	defer ai.Close()

	job := NewDailyBrief(db, marketingos.NewService(db, nil, zap.NewNop()),
		notifications.New(notifications.Config{BaseURL: notif.URL}), ai.URL, "svc", "https://portal", zap.NewNop())
	// In-memory stand-in for Redis ClaimOnce (no Redis in local tests).
	claimed := map[string]bool{}
	job.claim = func(_ context.Context, key string, _ time.Duration) bool {
		if claimed[key] {
			return false
		}
		claimed[key] = true
		return true
	}

	// Before 06:30 local nothing goes out.
	job.nowFn = func() time.Time { return time.Date(2026, 10, 22, 6, 0, 0, 0, loc) }
	if ok, err := job.forTenant(ctx, tid); err != nil || ok {
		t.Fatalf("too early: ok=%v err=%v", ok, err)
	}
	job.nowFn = func() time.Time { return now }
	ok, err := job.forTenant(ctx, tid)
	if err != nil || !ok {
		t.Fatalf("brief not sent: ok=%v err=%v", ok, err)
	}
	if len(sent) != 1 || sent[0]["recipient"] != "owner@example.com" || !strings.Contains(sent[0]["body"].(string), "Call the new lead today.") {
		t.Fatalf("sent = %+v", sent)
	}
	// Same local day again: claimed, nothing more is sent.
	if ok, _ := job.forTenant(ctx, tid); ok || len(sent) != 1 {
		t.Fatalf("sent twice in one day (%d)", len(sent))
	}

	// Opted out: nothing on the next day.
	if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{DailyBriefOptOutKey: false}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	job.nowFn = func() time.Time { return now.AddDate(0, 0, 1) }
	if ok, _ := job.forTenant(ctx, tid); ok || len(sent) != 1 {
		t.Fatal("opted-out tenant received a brief")
	}
}
