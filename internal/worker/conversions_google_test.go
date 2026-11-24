package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

// A conversion against a fake Google: sent through the Data Manager API with the account's own
// action, tracked, and resolved from Google's later answer; an account without the permission is
// not called and says so; the old API still works behind the switch.
func TestGoogleConversionThroughDataManager(t *testing.T) {
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
	if _, err := db.Tenant.Create().SetID(tid).SetName("DM " + tid.String()).SetSlug("dm-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	key, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys := encryption.NewKeyProvider(nil, key)
	sealed, err := keys.Encrypt(ctx, "tok")
	if err != nil {
		t.Fatal(err)
	}
	newAccount := func(ext string, scopes []string) *ent.ConnectedAccount {
		a, err := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("google").SetAccountType(googleads.AccountType).SetExternalID(ext).
			SetDisplayName(ext).SetStatus("active").SetTokenCiphertext(sealed).SetScopes(scopes).
			SetMetadata(map[string]any{"currency": "KES", google.MetaAccessExpiresAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
				googleads.MetaConversionActions: map[string]any{"Purchase": "customers/" + ext + "/conversionActions/77"}}).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	full := newAccount("1112223334", google.GoogleAdsConnectScopes)
	old := newAccount("5556667778", []string{google.ScopeAdWords})

	var sent []string
	status := `{"requestStatusPerDestination":[{"requestStatus":"PROCESSING"}]}`
	ingestStatus, ingestBody := 200, `{"requestId":"req-1"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/events:ingest":
			sent = append(sent, string(b))
			w.WriteHeader(ingestStatus)
			_, _ = io.WriteString(w, ingestBody)
		case r.URL.Path == "/requestStatus:retrieve":
			_, _ = io.WriteString(w, status)
		case strings.HasSuffix(r.URL.Path, ":uploadClickConversions"):
			sent = append(sent, r.URL.Path+" "+string(b))
			_, _ = io.WriteString(w, `{"partialFailureError":{"message":"CUSTOMER_NOT_ALLOWLISTED_FOR_THIS_FEATURE"}}`)
		default:
			t.Errorf("unexpected call %s", r.URL)
		}
	}))
	defer srv.Close()
	h := &ConversionsHandler{db: db, keys: keys, logger: zap.NewNop(), http: srv.Client(), dataManagerBase: srv.URL, googleAdsBase: srv.URL,
		googleSettings: func(context.Context) (map[string]string, error) { return map[string]string{}, nil }}
	ev := &ConversionEvent{TenantID: tid.String(), EventName: "Purchase", Value: 2500, OccurredAt: "2026-11-25T09:00:00Z", Gclid: "G1", ConversionID: "tx-1"}
	read := func(a *ent.ConnectedAccount) *ent.ConnectedAccount {
		a, err := db.ConnectedAccount.Get(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	done, err := h.uploadToConnectedAccounts(ctx, ev, googleads.UploadAPIDataManager)
	if !done || err == nil || !strings.Contains(err.Error(), "Reconnect Google Ads") {
		t.Errorf("the account without the permission is reported: %v %v", done, err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], `"transactionId":"tx-1"`) || !strings.Contains(sent[0], `"currency":"KES"`) ||
		!strings.Contains(sent[0], `"accountId":"1112223334"`) || !strings.Contains(sent[0], `"productDestinationId":"77"`) {
		t.Fatalf("sent %v", sent)
	}
	if u := googleads.LastUpload(read(old)); u == nil || u.Status != googleads.UploadReconnect {
		t.Errorf("old connection: %+v", u)
	}
	a := read(full)
	p, ok := googleads.PendingRequests(a)["req-1"]
	if u := googleads.LastUpload(a); u == nil || u.Status != googleads.UploadSent || !ok || p.Next.Sub(p.At) != 30*time.Minute {
		t.Fatalf("tracked: %+v %+v", u, p)
	}
	if googleads.MappingOf(a)["Purchase"] == "" || a.Metadata["currency"] != "KES" {
		t.Error("the other metadata stays")
	}

	// The diagnostics job: not due, still processing, then refused.
	now := time.Now()
	j := &ConversionDiagnostics{db: db, hc: srv.Client(), base: srv.URL, now: func() time.Time { return now }, logger: zap.NewNop(),
		tokens: google.NewTokensFor(db, keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName)}
	j.tokens.Now = time.Now
	check := func() *ent.ConnectedAccount {
		j.check(ctx, connectedaccount.TenantID(tid))
		return read(full)
	}
	if got := googleads.PendingRequests(check())["req-1"]; !got.Next.Equal(p.Next) {
		t.Error("asked before it was due")
	}
	now = now.Add(31 * time.Minute)
	if got := googleads.PendingRequests(check())["req-1"]; got.WaitMinutes != 39 || !got.Next.After(now) {
		t.Errorf("rescheduled: %+v", got)
	}
	now = now.Add(time.Hour)
	status = `{"requestStatusPerDestination":[{"requestStatus":"FAILED","errorInfo":{"errorCounts":[{"reason":"PROCESSING_ERROR_REASON_EXPIRED_CLICK"}]}}]}`
	a = check()
	if u := googleads.LastUpload(a); len(googleads.PendingRequests(a)) != 0 || u.Status != googleads.UploadFailed || !strings.Contains(u.Reason, "click window") {
		t.Errorf("resolved: %+v", u)
	}

	// A request with no final answer after a day is dropped and says so.
	if _, err := h.uploadToConnectedAccounts(ctx, ev, googleads.UploadAPIDataManager); err == nil {
		t.Error("the old connection still fails")
	}
	status = `{"requestStatusPerDestination":[{"requestStatus":"PROCESSING"}]}`
	now = time.Now().Add(25 * time.Hour)
	a = check()
	if u := googleads.LastUpload(a); len(googleads.PendingRequests(a)) != 0 || u.Status != googleads.UploadUnknown {
		t.Errorf("stopped: %+v", u)
	}

	// Google's refusal at once.
	ingestStatus, ingestBody = 403, `{"error":{"status":"PERMISSION_DENIED","details":[{"reason":"SERVICE_DISABLED"}]}}`
	_, _ = h.uploadToConnectedAccounts(ctx, ev, googleads.UploadAPIDataManager)
	if u := googleads.LastUpload(read(full)); u.Status != googleads.UploadFailed || !strings.Contains(u.Reason, "not switched on") {
		t.Errorf("refused: %+v", u)
	}

	// The switch back: the old call, for both accounts, with its partial failure in plain words.
	sent = nil
	if _, err := h.uploadToConnectedAccounts(ctx, ev, googleads.UploadAPIGoogleAds); err == nil || len(sent) != 2 || !strings.Contains(sent[0], `"conversionDateTime":"2026-11-25 09:00:00+00:00"`) {
		t.Errorf("google ads api: %v %v", sent, err)
	}
	if u := googleads.LastUpload(read(old)); u.Status != googleads.UploadFailed || !strings.Contains(u.Reason, "data_manager") {
		t.Errorf("allowlist: %+v", u)
	}

	// A reconnect keeps the mapping and forgets the old outcome.
	fresh := map[string]any{"currency": "USD"}
	googleads.KeepOnReconnect(read(full).Metadata, fresh)
	if b, _ := json.Marshal(fresh); !strings.Contains(string(b), "conversionActions/77") || strings.Contains(string(b), "conversion_upload") || fresh["currency"] != "USD" {
		t.Errorf("reconnect: %s", b)
	}
}
