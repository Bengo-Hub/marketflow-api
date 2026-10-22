package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

func TestTenantCheck(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	serve := func(enforce bool, claims *authclient.Claims, target string, hdr map[string]string) int {
		deps := RouterDeps{Log: zap.New(core), EnforceTenantHeader: enforce}
		r := chi.NewRouter()
		r.Use(tenantCheck(deps, ""))
		ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
		r.Get("/leads", ok)
		r.With(tenantCheck(deps, "tenantID")).Post("/{tenantID}/campaigns/{campaignID}/send-sms", ok)
		method := http.MethodGet
		if target != "/leads" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, target, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if claims != nil {
			req = req.WithContext(authclient.ContextWithClaims(req.Context(), claims))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	user := &authclient.Claims{TenantID: tenantA, TenantSlug: "acme"}
	owner := &authclient.Claims{TenantID: tenantA, TenantSlug: "codevertex", IsPlatformOwner: true}
	other := map[string]string{"X-Tenant-ID": tenantB}

	cases := []struct {
		name    string
		enforce bool
		claims  *authclient.Claims
		target  string
		hdr     map[string]string
		want    int
	}{
		{"own tenant", true, user, "/leads", map[string]string{"X-Tenant-ID": tenantA, "X-Tenant-Slug": "acme"}, 200},
		{"other tenant id, enforced", true, user, "/leads", other, 403},
		{"other tenant slug, enforced", true, user, "/leads", map[string]string{"X-Tenant-Slug": "rival"}, 403},
		{"other tenant in the path, enforced", true, user, "/" + tenantB + "/campaigns/c1/send-sms", nil, 403},
		{"own tenant in the path", true, user, "/" + tenantA + "/campaigns/c1/send-sms", nil, 200},
		{"platform owner acting for a tenant", true, owner, "/leads", other, 200},
		{"service call without claims", true, nil, "/leads", other, 200},
		{"other tenant id, report only", false, user, "/leads", other, 200},
	}
	for _, c := range cases {
		if got := serve(c.enforce, c.claims, c.target, c.hdr); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	// Every mismatch by a tenant user is logged (three enforced, one report-only); platform owners
	// and service calls are not.
	if n := logs.FilterMessage("tenant in request does not match the token").Len(); n != 4 {
		t.Errorf("mismatch logs = %d, want 4", n)
	}
}
