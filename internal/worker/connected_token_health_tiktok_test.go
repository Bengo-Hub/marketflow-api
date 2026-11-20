package worker

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/json"

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
	"github.com/bengobox/marketflow-api/internal/integrations/tiktok"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// TikTok accounts get the same daily renewal as YouTube channels: a refused refresh token marks
// the account expired and tells the admins to reconnect; a working one is renewed and stored.
func TestConnectedTokenHealthTikTok(t *testing.T) {
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

	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("TikTok health " + tid.String()).SetSlug("tt-health-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.UserRoleAssignment.Delete().Where(userroleassignment.TenantID(tid)).Exec(ctx)
		_, _ = db.MarketflowRole.Delete().Where(marketflowrole.TenantID(tid)).Exec(ctx)
		_, _ = db.MarketflowUser.Delete().Where(marketflowuser.TenantID(tid)).Exec(ctx)
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	admin, _ := db.MarketflowRole.Create().SetTenantID(tid).SetRoleCode("marketflow_admin").SetName("Admin").Save(ctx)
	u, err := db.MarketflowUser.Create().SetTenantID(tid).SetAuthServiceUserID(uuid.New()).SetEmail("owner@example.com").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserRoleAssignment.Create().SetTenantID(tid).SetUserID(u.ID).SetRoleID(admin.ID).SetAssignedBy(u.ID).Save(ctx); err != nil {
		t.Fatal(err)
	}

	key, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys := encryption.NewKeyProvider(nil, key)
	ids := map[string]uuid.UUID{}
	for name, refresh := range map[string]string{"Removed Shop": "RFT-DEAD", "Live Shop": "RFT-GOOD"} {
		sealedAccess, _ := keys.Encrypt(ctx, "ACT-OLD")
		sealedRefresh, _ := keys.Encrypt(ctx, refresh)
		acc, err := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("tiktok").SetAccountType(tiktok.AccountCreator).
			SetExternalID("open-" + refresh + "-" + tid.String()[:8]).SetDisplayName(name).
			SetTokenCiphertext(sealedAccess).SetRefreshCiphertext(sealedRefresh).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids[refresh] = acc.ID
	}

	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("refresh_token") == "RFT-DEAD" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Refresh token is invalid or expired.","log_id":"L1"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"access_token":"ACT-NEW","expires_in":86400,"refresh_token":"RFT-NEXT","refresh_expires_in":31536000,"open_id":"o","scope":"user.info.basic,video.publish,video.upload"}`)
	}))
	defer tokens.Close()

	var mu sync.Mutex
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

	j := NewConnectedTokenHealth(db, keys, notifications.New(notifications.Config{BaseURL: notif.URL, APIKey: "test"}), "https://portal.example.com/", zap.NewNop())
	j.appToken = func(context.Context) (string, bool) { return "", false } // Meta not part of this test
	j.youtube = nil
	j.tiktok.OAuth = tiktok.OAuth{TokenURL: tokens.URL}
	j.tiktok.App = func(context.Context) (tiktok.App, error) {
		return tiktok.App{Key: "k", Secret: "s", RedirectURL: "https://api.example.test/cb"}, nil // secret-scan:allow
	}

	if _, _, err := j.check(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	dead, _ := db.ConnectedAccount.Get(ctx, ids["RFT-DEAD"])
	if dead.Status != "expired" {
		t.Errorf("a refused refresh token marks the account expired, got %q", dead.Status)
	}
	live, _ := db.ConnectedAccount.Get(ctx, ids["RFT-GOOD"])
	if live.Status != "active" {
		t.Errorf("a renewed account stays active, got %q", live.Status)
	}
	if tok, _ := keys.Decrypt(ctx, live.TokenCiphertext); tok != "ACT-NEW" {
		t.Errorf("the renewed access token is stored, got %q", tok)
	}
	if rt, _ := keys.Decrypt(ctx, live.RefreshCiphertext); rt != "RFT-NEXT" {
		t.Errorf("a rotated refresh token is stored, got %q", rt)
	}
	if tiktok.AccessExpiry(live).IsZero() {
		t.Error("the new access expiry is recorded")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || firstTo(sent[0]) != "owner@example.com" {
		t.Fatalf("the admin is told once: %+v", sent)
	}
	body := dataBody(sent[0])
	if !strings.Contains(body, "Removed Shop") || strings.Contains(body, "Live Shop") ||
		!strings.Contains(body, "/tt-health-"+tid.String()+"/settings/integrations") {
		t.Errorf("notice names only the expired account, with the reconnect link: %q", body)
	}
}
