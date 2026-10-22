package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowrole"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/bengobox/marketflow-api/internal/ent/userroleassignment"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

func TestRenewDate(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	in3, in90 := now.Add(72*time.Hour), now.Add(90*24*time.Hour)
	cases := []struct {
		name string
		st   tokenStatus
		want time.Time
	}{
		{"token expiry first", tokenStatus{Valid: true, ExpiresAt: in3, DataUntil: in90}, in3},
		{"data access only (page token)", tokenStatus{Valid: true, DataUntil: in90}, in90},
		{"neither: recheck later", tokenStatus{Valid: true}, now.Add(tokenRecheckAfter)},
	}
	for _, c := range cases {
		if got := renewDate(c.st, now); !got.Equal(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConnectedTokenHealth(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWorkerPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	defer sqlDB.Close()
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("Health " + tid.String()).SetSlug("health-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = db.UserRoleAssignment.Delete().Where(userroleassignment.TenantID(tid)).Exec(ctx)
		_, _ = db.MarketflowRole.Delete().Where(marketflowrole.TenantID(tid)).Exec(ctx)
		_, _ = db.MarketflowUser.Delete().Where(marketflowuser.TenantID(tid)).Exec(ctx)
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})

	// One admin who must be told, one viewer who must not.
	admin, _ := db.MarketflowRole.Create().SetTenantID(tid).SetRoleCode("marketflow_admin").SetName("Admin").Save(ctx)
	viewer, _ := db.MarketflowRole.Create().SetTenantID(tid).SetRoleCode("viewer").SetName("Viewer").Save(ctx)
	for email, role := range map[string]*ent.MarketflowRole{"owner@example.com": admin, "viewer@example.com": viewer} {
		u, err := db.MarketflowUser.Create().SetTenantID(tid).SetAuthServiceUserID(uuid.New()).SetEmail(email).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.UserRoleAssignment.Create().SetTenantID(tid).SetUserID(u.ID).SetRoleID(role.ID).SetAssignedBy(u.ID).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}

	key, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys := encryption.NewKeyProvider(nil, key)
	ids := map[string]uuid.UUID{}
	for name, tok := range map[string]string{"Dead Page": "TOK-DEAD", "Ads": "TOK-SOON", "Shop": "TOK-PAGE"} {
		sealed, _ := keys.Encrypt(ctx, tok)
		acc, err := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("meta").SetAccountType("facebook_page").
			SetExternalID("x-" + tok).SetDisplayName(name).SetTokenCiphertext(sealed).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids[tok] = acc.ID
	}

	soon, page := now.Add(72*time.Hour).Truncate(time.Second), now.Add(90*24*time.Hour).Truncate(time.Second)
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("access_token") != "APP|SECRET" {
			http.Error(w, `{"error":{"message":"bad app token"}}`, http.StatusBadRequest)
			return
		}
		data := map[string]any{"is_valid": true}
		switch r.URL.Query().Get("input_token") {
		case "TOK-DEAD":
			data["is_valid"] = false
		case "TOK-SOON":
			data["expires_at"] = soon.Unix()
		case "TOK-PAGE":
			data["expires_at"] = 0
			data["data_access_expires_at"] = page.Unix()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer graph.Close()

	var mu sync.Mutex
	var sent []map[string]any
	var keysSeen []string
	notif := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		if strings.Contains(r.URL.Path, tid.String()) {
			sent = append(sent, m)
			keysSeen = append(keysSeen, r.Header.Get("Idempotency-Key"))
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"status":"queued"}`)
	}))
	defer notif.Close()

	j := NewConnectedTokenHealth(db, keys, notifications.New(notifications.Config{BaseURL: notif.URL, APIKey: "test"}), "https://portal.example.com/", zap.NewNop())
	j.graphBase = graph.URL
	j.appToken = func(context.Context) (string, bool) { return "APP|SECRET", true }

	if _, _, err := j.check(ctx, now); err != nil {
		t.Fatal(err)
	}
	get := func(tok string) *ent.ConnectedAccount {
		acc, err := db.ConnectedAccount.Get(ctx, ids[tok])
		if err != nil {
			t.Fatal(err)
		}
		return acc
	}
	if a := get("TOK-DEAD"); a.Status != "expired" {
		t.Errorf("an invalid token is marked expired, got %q", a.Status)
	}
	if a := get("TOK-SOON"); a.Status != "active" || a.ExpiresAt == nil || !a.ExpiresAt.Equal(soon) {
		t.Errorf("token expiry recorded: %+v", a.ExpiresAt)
	}
	if a := get("TOK-PAGE"); a.ExpiresAt == nil || !a.ExpiresAt.Equal(page) {
		t.Errorf("a page token is rechecked at its data-access expiry: %+v", a.ExpiresAt)
	}

	mu.Lock()
	if len(sent) != 1 || sent[0]["recipient"] != "owner@example.com" {
		t.Fatalf("only the admin is emailed, once: %+v", sent)
	}
	body, _ := sent[0]["body"].(string)
	if !strings.Contains(body, "Dead Page") || !strings.Contains(body, "Ads") || strings.Contains(body, "Shop") ||
		!strings.Contains(body, "https://portal.example.com/health-"+tid.String()+"/settings/integrations") {
		t.Errorf("notice lists the expired and expiring accounts with the reconnect link: %q", body)
	}
	mu.Unlock()

	// The next day: the expired account is no longer checked, the expiring one is still in the
	// window, and the notice repeats under the same idempotency key (notifications-api drops it).
	if _, _, err := j.check(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keysSeen) != 2 || keysSeen[0] == "" {
		t.Fatalf("second run notices: %v", keysSeen)
	}
	if b, _ := sent[1]["body"].(string); strings.Contains(b, "Dead Page") {
		t.Error("an expired account is not checked again")
	}
}
