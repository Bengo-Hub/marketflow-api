package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"go.uber.org/zap"
)

func gated(fg *FeatureGate, claims *authclient.Claims, hdrTenant string) *httptest.ResponseRecorder {
	h := fg.RequireFeature("ai_chat_messages", "AI credits exhausted")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-Tenant-ID", hdrTenant)
	if claims != nil {
		req = req.WithContext(authclient.ContextWithClaims(req.Context(), claims))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFeatureGateAllowsBlocksAndUsesTokenTenant(t *testing.T) {
	var seenTenant atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenTenant.Store(r.Header.Get("X-Tenant-ID"))
		if r.Header.Get("X-Tenant-ID") == "over" {
			_, _ = w.Write([]byte(`{"allowed":false,"limit_value":100,"current_usage":100}`))
			return
		}
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	defer srv.Close()
	fg := NewFeatureGate(srv.URL, "k", zap.NewNop())

	if rec := gated(fg, &authclient.Claims{TenantID: "over"}, "over"); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("over quota: %d", rec.Code)
	}
	// A header naming a tenant with quota does not let an over-quota user through.
	if rec := gated(fg, &authclient.Claims{TenantID: "over"}, "rich"); rec.Code != http.StatusPaymentRequired || seenTenant.Load() != "over" {
		t.Fatalf("the token's tenant is checked, not the header's: %d %v", rec.Code, seenTenant.Load())
	}
	if rec := gated(fg, &authclient.Claims{TenantID: "platform", IsService: true}, "over"); rec.Code != http.StatusPaymentRequired || seenTenant.Load() != "over" {
		t.Fatalf("a platform service key is checked against the tenant it acts for: %d %v", rec.Code, seenTenant.Load())
	}
	if rec := gated(fg, nil, "rich"); rec.Code != http.StatusOK {
		t.Fatalf("a service call keeps the header tenant: %d", rec.Code)
	}
}

func TestFeatureGateFailsOpenFast(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select { // a subscriptions-api that hangs
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	fg := NewFeatureGate(srv.URL, "k", zap.NewNop())

	start := time.Now()
	if ok, _ := fg.Check(context.Background(), "t1", "x"); !ok {
		t.Fatal("a hanging service fails open")
	}
	if d := time.Since(start); d > featureCheckBudget+500*time.Millisecond {
		t.Fatalf("one check took %v, budget %v", d, featureCheckBudget)
	}
	// Two more failures open the breaker; from then on a check does not call at all.
	_, _ = fg.Check(context.Background(), "t1", "x")
	_, _ = fg.Check(context.Background(), "t1", "x")
	before := calls.Load()
	start = time.Now()
	if ok, _ := fg.Check(context.Background(), "t1", "x"); !ok {
		t.Fatal("an open breaker fails open")
	}
	if d := time.Since(start); d > 100*time.Millisecond || calls.Load() != before {
		t.Fatalf("an open breaker must answer at once without calling: %v, calls %d->%d", d, before, calls.Load())
	}
	// No subscriptions URL: always open, no calls.
	if ok, _ := NewFeatureGate("", "", zap.NewNop()).Check(context.Background(), "t1", "x"); !ok {
		t.Fatal("an unconfigured gate is open")
	}
}
