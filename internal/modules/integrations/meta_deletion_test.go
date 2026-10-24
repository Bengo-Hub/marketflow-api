package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/auditlog"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
)

// signRequest builds a signed_request the way Meta does.
func signRequest(secret string, payload map[string]any) string {
	raw, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) + "." + body
}

func TestParseSignedRequest(t *testing.T) {
	ok := signRequest("s3cret", map[string]any{"algorithm": "HMAC-SHA256", "user_id": "123", "issued_at": 1})
	p, err := parseSignedRequest(ok, "s3cret")
	if err != nil || p["user_id"] != "123" {
		t.Fatalf("valid request: %v %v", p, err)
	}
	if _, err := parseSignedRequest(ok, "other"); err == nil {
		t.Fatal("a request signed with another secret was accepted")
	}
	tampered := strings.Split(ok, ".")[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"algorithm":"HMAC-SHA256","user_id":"999"}`))
	if _, err := parseSignedRequest(tampered, "s3cret"); err == nil {
		t.Fatal("a tampered payload was accepted")
	}
	if _, err := parseSignedRequest(signRequest("s3cret", map[string]any{"algorithm": "none", "user_id": "1"}), "s3cret"); err == nil {
		t.Fatal("a non HMAC-SHA256 algorithm was accepted")
	}
	if _, err := parseSignedRequest("nodot", "s3cret"); err == nil {
		t.Fatal("garbage was accepted")
	}
}

// The callback deletes only what the named person granted, unlinks the tenant's
// Page, answers Meta's {url, confirmation_code}, and the code's status resolves.
func TestMetaDataDeletionCallback(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	defer sqlDB.Close()
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer db.Close()
	ctx := context.Background()

	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("MD " + tid.String()).SetSlug("md-" + tid.String()).SetMetaPageID("p-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	fbUser, other := "fbu-"+tid.String(), "fbu-other-"+tid.String()
	t.Cleanup(func() {
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
		_, _ = db.ProviderSetting.Delete().Where(providersetting.TenantIDEQ(tid.String())).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	for i, who := range []string{fbUser, other} {
		db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform(PlatformMeta).SetAccountType(AccountFacebookPage).
			SetExternalID("page-" + string(rune('a'+i))).SetTokenCiphertext("x").
			SetMetadata(map[string]any{MetaFBUserKey: who}).SaveX(ctx)
	}
	for k, v := range map[string]string{"page_id": "p1", MetaFBUserKey: fbUser} {
		db.ProviderSetting.Create().SetTenantID(tid.String()).SetProviderType("meta_ads").SetProviderName("page").
			SetKey(k).SetValue(v).SaveX(ctx)
	}

	h := (&Handler{db: db, keys: encryption.NewKeyProvider(nil, ""), logger: zap.NewNop()}).WithMeta("app-secret", "https://mf.example/data-deletion")
	form := url.Values{"signed_request": {signRequest("app-secret", map[string]any{"algorithm": "HMAC-SHA256", "user_id": fbUser})}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.MetaDataDeletion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deletion = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		URL  string `json:"url"`
		Code string `json:"confirmation_code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Code == "" || out.URL != "https://mf.example/data-deletion?code="+out.Code {
		t.Fatalf("answer: %s", rec.Body)
	}
	t.Cleanup(func() {
		_, _ = db.AuditLog.Delete().Where(auditlog.EventEQ(eventMetaDeletion), auditlog.SubjectTypeEQ("meta_user"),
			func(s *entsql.Selector) { s.Where(entsql.ExprP("payload->>'confirmation_code' = $1", out.Code)) }).Exec(ctx)
	})

	left, _ := db.ConnectedAccount.Query().Where(connectedaccount.TenantID(tid)).All(ctx)
	if len(left) != 1 || left[0].Metadata[MetaFBUserKey] != other {
		t.Fatalf("only the named person's accounts go: %d left", len(left))
	}
	if n, _ := db.ProviderSetting.Query().Where(providersetting.TenantIDEQ(tid.String())).Count(ctx); n != 0 {
		t.Fatalf("page settings left: %d", n)
	}
	if tn := db.Tenant.GetX(ctx, tid); tn.MetaPageID != nil {
		t.Fatalf("the tenant's Page is still linked: %v", *tn.MetaPageID)
	}

	// The status page resolves the code; an unknown code is 404.
	status := func(code string) int {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("code", code)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
		w := httptest.NewRecorder()
		h.MetaDeletionStatus(w, r)
		return w.Code
	}
	if c := status(out.Code); c != http.StatusOK {
		t.Fatalf("status of a real code = %d", c)
	}
	if c := status("DEADBEEFDEADBEEF"); c != http.StatusNotFound {
		t.Fatalf("status of an unknown code = %d", c)
	}

	// A forged request changes nothing.
	bad := url.Values{"signed_request": {signRequest("wrong", map[string]any{"algorithm": "HMAC-SHA256", "user_id": other})}}
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(bad.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.MetaDeauthorize(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged deauthorize = %d", rec.Code)
	}
	if n, _ := db.ConnectedAccount.Query().Where(connectedaccount.TenantID(tid)).Count(ctx); n != 1 {
		t.Fatal("a forged request deleted data")
	}
}
