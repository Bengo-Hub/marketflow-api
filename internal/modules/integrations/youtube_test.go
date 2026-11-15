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
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
)

func TestYouTubeAccounts(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	got := youtubeAccounts([]google.Channel{{ID: "UC1", Title: "Shop TV", CustomURL: "@shoptv", Thumbnail: "https://yt3/m.jpg"}},
		google.Token{Access: "AT", Refresh: "RT", ExpiresIn: time.Hour, Scopes: publishing.YouTubeConnectScopes}, now)
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	a := got[0]
	if a.Platform != PlatformGoogle || a.Type != AccountYouTubeChannel || a.ExternalID != "UC1" || a.DisplayName != "Shop TV" ||
		a.Token != "AT" || a.Refresh != "RT" || len(a.Scopes) != 2 {
		t.Errorf("channel row: %+v", a)
	}
	if a.Metadata[google.MetaAccessExpiresAt] != "2026-11-15T10:00:00Z" || a.Metadata["picture_url"] != "https://yt3/m.jpg" || a.Metadata["handle"] != "@shoptv" {
		t.Errorf("metadata: %+v", a.Metadata)
	}
	// The channel shows on the YouTube card, never under Meta, TikTok or a later Google Ads card.
	if p := publishing.PlatformForAccountType(a.Type); p != publishing.YouTube {
		t.Errorf("the publish dialog posts a channel to YouTube, got %q", p)
	}
	by := accountSummaries([]*ent.ConnectedAccount{{ID: uuid.New(), Platform: PlatformGoogle, AccountType: AccountYouTubeChannel,
		ExternalID: "UC1", DisplayName: "Shop TV", Status: "active", Metadata: a.Metadata}}, "", nil, now)
	if list := by[providerYouTube]; len(list) != 1 || list[0].Health != healthOK || list[0].PictureURL != "https://yt3/m.jpg" {
		t.Errorf("summary: %+v", by)
	}
	if by[""] != nil {
		t.Error("no account without a provider")
	}
}

// The whole connect against a fake Google: the channel lands as one ConnectedAccount row with
// both tokens as ciphertext, the card lists it, a token renewal works from the stored refresh
// token, and a disconnect revokes the grant and drops the tokens.
func TestYouTubeConnectFlow(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
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
	slug := "yt-" + tid.String()
	if _, err := db.Tenant.Create().SetID(tid).SetName("YT " + tid.String()).SetSlug(slug).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})

	key, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys := encryption.NewKeyProvider(nil, key)

	// The platform client is a shared row; only seed it when this database has none, and
	// remove only what this test created.
	existing, _ := db.ProviderSetting.Query().Where(providersetting.TenantIDEQ("platform"),
		providersetting.ProviderTypeEQ(google.ProviderYouTube), providersetting.ProviderNameEQ("oauth")).Count(ctx)
	if existing > 0 {
		t.Skip("this database already has a platform youtube client; not overwriting it")
	}
	for k, v := range map[string]string{"client_id": "cid", "client_secret": "csec", "redirect_url": "https://api.example.com/api/v1/integrations/youtube/callback"} {
		row, err := db.ProviderSetting.Create().SetTenantID("platform").SetProviderType(google.ProviderYouTube).SetProviderName("oauth").
			SetEnvironment("production").SetKey(k).SetValue(v).SetIsActive(true).SetStatus("active").Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id := row.ID
		t.Cleanup(func() { _ = db.ProviderSetting.DeleteOneID(id).Exec(ctx) })
	}

	var revokedTokens []string
	scope := strings.Join(publishing.YouTubeConnectScopes, " ")
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case r.URL.Path == "/token" && r.Form.Get("code") == "partial":
			_, _ = w.Write([]byte(`{"access_token":"AT0","refresh_token":"RT0","expires_in":3599,"scope":"https://www.googleapis.com/auth/youtube.upload"}`))
		case r.URL.Path == "/token" && r.Form.Get("code") == "good" && r.Form.Get("client_secret") == "csec":
			_, _ = w.Write([]byte(`{"access_token":"AT1","refresh_token":"RT1","expires_in":3599,"scope":"` + scope + `"}`))
		case r.URL.Path == "/token" && r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "RT1":
			_, _ = w.Write([]byte(`{"access_token":"AT2","expires_in":3599}`))
		case r.URL.Path == "/revoke":
			revokedTokens = append(revokedTokens, r.Form.Get("token"))
		case r.URL.Path == "/youtube/v3/channels" && r.Header.Get("Authorization") == "Bearer AT1":
			_, _ = w.Write([]byte(`{"items":[{"id":"UC1","snippet":{"title":"Shop TV","thumbnails":{"default":{"url":"https://yt3/d.jpg"}}}}]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		}
	}))
	defer fake.Close()

	h := NewHandler(db, []byte("0123456789abcdef0123456789abcdef"), keys, zap.NewNop()).WithPortal("https://portal.example.com")
	h.googleOAuth = google.OAuth{TokenURL: fake.URL + "/token", RevokeURL: fake.URL + "/revoke"}
	h.youtubeAPIBase = fake.URL + "/youtube/v3"

	callback := func(code string) *url.URL {
		rec := httptest.NewRecorder()
		q := url.Values{"code": {code}, "state": {h.signState(tid.String(), "")}}
		h.YouTubeCallback(rec, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/youtube/callback?"+q.Encode(), nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("callback answered %d", rec.Code)
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		return loc
	}

	// An unticked permission: nothing stored, the grant handed back.
	loc := callback("partial")
	if loc.Query().Get("youtube") != "error" || loc.Query().Get("reason") != "missing_scope" {
		t.Fatalf("partial grant: %s", loc)
	}
	if len(revokedTokens) != 1 || revokedTokens[0] != "RT0" {
		t.Errorf("a partial grant is revoked: %v", revokedTokens)
	}

	loc = callback("good")
	if loc.Query().Get("youtube") != "connected" || loc.Query().Get("channel") != "Shop TV" || loc.Path != "/"+slug+"/settings/integrations" {
		t.Fatalf("connected: %s", loc)
	}
	acc, err := db.ConnectedAccount.Query().Where(connectedaccount.TenantID(tid), connectedaccount.AccountType(AccountYouTubeChannel)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ExternalID != "UC1" || acc.Platform != PlatformGoogle || acc.ExpiresAt != nil ||
		acc.TokenCiphertext == "" || acc.TokenCiphertext == "AT1" || acc.RefreshCiphertext == "" || acc.RefreshCiphertext == "RT1" {
		t.Fatalf("stored channel: %+v", acc)
	}
	if rt, _ := keys.Decrypt(ctx, acc.RefreshCiphertext); rt != "RT1" {
		t.Errorf("refresh token round trip: %q", rt)
	}

	// The publisher renews the access token from the stored refresh token.
	tokens := google.NewTokens(db, keys)
	tokens.OAuth = h.googleOAuth
	if at, err := tokens.Refresh(ctx, acc); err != nil || at != "AT2" {
		t.Fatalf("renew: %q %v", at, err)
	}
	acc, _ = db.ConnectedAccount.Get(ctx, acc.ID)
	if at, err := tokens.AccessToken(ctx, acc); err != nil || at != "AT2" {
		t.Errorf("a fresh token is reused, not renewed again: %q %v", at, err)
	}
	if acc.Metadata["picture_url"] != "https://yt3/d.jpg" {
		t.Errorf("connect-time metadata kept across a renewal: %+v", acc.Metadata)
	}

	// The integrations card lists the channel though YouTube keeps no tenant ProviderSetting rows.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
	req.Header.Set("X-Tenant-ID", tid.String())
	h.ListTenant(rec, req)
	if !strings.Contains(rec.Body.String(), `"provider":"youtube"`) || !strings.Contains(rec.Body.String(), `"external_id":"UC1"`) {
		t.Errorf("list: %s", rec.Body.String())
	}

	// Disconnect: the grant is revoked at Google and the tokens dropped.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/integrations/youtube", nil)
	req.Header.Set("X-Tenant-ID", tid.String())
	rc := chi.NewRouteContext()
	rc.URLParams.Add("provider", "youtube")
	h.DisconnectTenant(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc)))
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", rec.Code, rec.Body.String())
	}
	if len(revokedTokens) != 2 || revokedTokens[1] != "RT1" {
		t.Errorf("disconnect revokes the stored grant: %v", revokedTokens)
	}
	acc, _ = db.ConnectedAccount.Get(ctx, acc.ID)
	if acc.Status != "revoked" || acc.TokenCiphertext != "" || acc.RefreshCiphertext != "" {
		t.Errorf("disconnected channel keeps no token: %+v", acc)
	}

	// A refused renewal marks the channel expired for the card and the publish dialog.
	acc2, err := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform(PlatformGoogle).SetAccountType(AccountYouTubeChannel).
		SetExternalID("UC2").SetRefreshCiphertext(mustSeal(t, keys, "RT-GONE")).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.AccessToken(ctx, acc2); err == nil {
		t.Fatal("a revoked grant must not hand out a token")
	}
	if acc2, _ = db.ConnectedAccount.Get(ctx, acc2.ID); acc2.Status != "expired" {
		t.Errorf("a refused grant marks the channel expired: %s", acc2.Status)
	}
}

func mustSeal(t *testing.T, keys *encryption.KeyProvider, s string) string {
	t.Helper()
	v, err := keys.Encrypt(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
