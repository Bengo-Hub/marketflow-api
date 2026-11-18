package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"go.uber.org/zap"
)

// Billing isolation (sprint audit item 4b): a usage event is charged to the tenant the request
// acted for, never to a tenant a visitor names in a header.

const (
	usageTenantA = "11111111-1111-1111-1111-111111111111"
	usageTenantB = "22222222-2222-2222-2222-222222222222"
)

// fakeSubscriptions records the tenant of every usage report it receives.
func fakeSubscriptions(t *testing.T) (*UsagePublisher, <-chan string) {
	t.Helper()
	got := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("X-Tenant-ID")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return NewUsagePublisher(srv.URL, "k", zap.NewNop()), got
}

// served runs one request through TrackUsage. acted is the tenant the handler records (empty for
// none), status its answer, header the X-Tenant-ID the caller sent, claims the sign-in (nil for a
// public route).
func served(u *UsagePublisher, acted string, status int, header string, claims *authclient.Claims) {
	h := u.TrackUsage("leads_captured")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acted != "" {
			SetUsageTenant(r.Context(), acted)
		}
		w.WriteHeader(status)
	}))
	req := httptest.NewRequest(http.MethodPost, "/public/leads/acme", nil)
	if header != "" {
		req.Header.Set("X-Tenant-ID", header)
	}
	if claims != nil {
		req = req.WithContext(authclient.ContextWithClaims(req.Context(), claims))
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func nextReport(got <-chan string) (string, bool) {
	select {
	case tid := <-got:
		return tid, true
	case <-time.After(500 * time.Millisecond):
		return "", false
	}
}

func TestUsageChargesTheTenantTheHandlerActedFor(t *testing.T) {
	u, got := fakeSubscriptions(t)
	// A visitor on tenant A's public page names tenant B in the header.
	served(u, usageTenantA, http.StatusCreated, usageTenantB, nil)
	if tid, ok := nextReport(got); !ok || tid != usageTenantA {
		t.Fatalf("the lead is tenant A's, so A is charged; got %q (reported %v)", tid, ok)
	}
}

func TestUsagePublicHeaderAloneChargesNobody(t *testing.T) {
	u, got := fakeSubscriptions(t)
	served(u, "", http.StatusCreated, usageTenantB, nil)
	if tid, ok := nextReport(got); ok {
		t.Fatalf("a public request's own header must never be charged, got a report for %q", tid)
	}
}

func TestUsageSignedInUsesThePinnedHeader(t *testing.T) {
	u, got := fakeSubscriptions(t)
	// On a signed-in route tenantCheck has already pinned X-Tenant-ID to the caller's tenant.
	served(u, "", http.StatusCreated, usageTenantA, &authclient.Claims{TenantID: usageTenantA})
	if tid, ok := nextReport(got); !ok || tid != usageTenantA {
		t.Fatalf("a signed-in request charges its own tenant; got %q (reported %v)", tid, ok)
	}
}

func TestUsageNothingOnFailureOrBadTenant(t *testing.T) {
	u, got := fakeSubscriptions(t)
	served(u, usageTenantA, http.StatusBadRequest, "", nil)
	served(u, "not-a-uuid", http.StatusCreated, "", nil)
	served(u, "", http.StatusCreated, "undefined", &authclient.Claims{TenantID: usageTenantA})
	if tid, ok := nextReport(got); ok {
		t.Fatalf("a failed request or an unreadable tenant is never charged, got %q", tid)
	}
}

func TestSetUsageTenantWithoutTrackingIsHarmless(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	SetUsageTenant(req.Context(), usageTenantA) // no TrackUsage on the route: nothing to set, no panic
}
