package integrations

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/modules/googleads"
)

// The Google Ads connect against a fake Google: both scopes are asked for, unticking only the
// conversions permission still connects and says so, and a reconnect keeps the mapping.
func TestGoogleAdsConnectFlow(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	admin, err := sql.Open("pgx", testPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := admin.Ping(); err != nil {
		_ = admin.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	defer admin.Close()
	ctx := context.Background()
	// Its own schema, so a developer database's platform google_ads row is never touched.
	schemaName := "gads_" + uuid.NewString()[:8]
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schemaName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schemaName+" CASCADE") })
	sqlDB, err := sql.Open("pgx", testPostgresDSN+"&search_path="+schemaName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	if err := db.Schema.Create(ctx); err != nil {
		t.Fatal(err)
	}
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("GA").SetSlug("ga-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"client_id": "cid", "client_secret": "csec", "redirect_url": "https://api.example.com/cb"} {
		if err := db.ProviderSetting.Create().SetTenantID("platform").SetProviderType(google.ProviderGoogleAds).SetProviderName(google.ProviderGoogleAdsName).
			SetEnvironment("production").SetKey(k).SetValue(v).SetIsActive(true).SetStatus("active").Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	key, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	var revoked int
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		scopes := map[string]string{"both": google.ScopeAdWords + " " + google.ScopeDataManager, "ads": google.ScopeAdWords, "dm": google.ScopeDataManager}
		switch {
		case r.URL.Path == "/token":
			_, _ = w.Write([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":3599,"scope":"` + scopes[r.Form.Get("code")] + `"}`))
		case r.URL.Path == "/revoke":
			revoked++
		case strings.HasSuffix(r.URL.Path, "customers:listAccessibleCustomers"):
			_, _ = w.Write([]byte(`{"resourceNames":["customers/2018958413"]}`))
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			_, _ = w.Write([]byte(`{"results":[{"customer":{"descriptiveName":"Shop","currencyCode":"KES"}}]}`))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer fake.Close()
	h := NewHandler(db, []byte("0123456789abcdef0123456789abcdef"), encryption.NewKeyProvider(nil, key), zap.NewNop()).WithPortal("https://portal.example.com")
	h.googleOAuth = google.OAuth{TokenURL: fake.URL + "/token", RevokeURL: fake.URL + "/revoke"}
	h.googleAdsBase = fake.URL

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/google-ads/authorize", nil)
	req.Header.Set("X-Tenant-ID", tid.String())
	h.GoogleAdsAuthorize(rec, req)
	if b := rec.Body.String(); !strings.Contains(b, "auth%2Fadwords+https") || !strings.Contains(b, "auth%2Fdatamanager") {
		t.Errorf("consent url: %d %s", rec.Code, b)
	}

	connect := func(code string) url.Values {
		rec := httptest.NewRecorder()
		q := url.Values{"code": {code}, "state": {h.signState(tid.String(), "")}}
		h.GoogleAdsCallback(rec, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/google-ads/callback?"+q.Encode(), nil))
		loc, _ := url.Parse(rec.Header().Get("Location"))
		return loc.Query()
	}
	row := func() *ent.ConnectedAccount {
		a, err := db.ConnectedAccount.Query().Where(connectedaccount.TenantID(tid)).Only(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	// Only the conversions permission unticked: connected, and the page is told.
	if q := connect("ads"); q.Get("google_ads") != "connected" || q.Get("conversions") != "off" || revoked != 0 {
		t.Fatalf("ads only: %v", q)
	}
	a := row()
	if googleads.MaySendConversions(googleads.UploadAPIDataManager, a.Scopes) {
		t.Error("the missing permission is recorded")
	}
	mapping := map[string]any{"Purchase": "customers/2018958413/conversionActions/7"}
	if err := googleads.PatchMetadata(ctx, db, a.ID, map[string]any{googleads.MetaConversionActions: mapping}); err != nil {
		t.Fatal(err)
	}
	if err := googleads.SaveUpload(ctx, db, a.ID, googleads.Upload{Status: googleads.UploadReconnect}); err != nil {
		t.Fatal(err)
	}

	// The reconnect: the permission is there, the mapping stays and the old complaint is gone.
	if q := connect("both"); q.Get("google_ads") != "connected" || q.Has("conversions") {
		t.Fatalf("both: %v", q)
	}
	a = row()
	if !googleads.MaySendConversions(googleads.UploadAPIDataManager, a.Scopes) || googleads.MappingOf(a)["Purchase"] == "" || googleads.LastUpload(a) != nil {
		t.Errorf("after the reconnect: %v %v", a.Scopes, a.Metadata)
	}

	// Without the Google Ads permission nothing works, so that still fails.
	if q := connect("dm"); q.Get("google_ads") != "error" || q.Get("reason") != "missing_scope" || revoked != 1 {
		t.Errorf("no adwords: %v", q)
	}
}
