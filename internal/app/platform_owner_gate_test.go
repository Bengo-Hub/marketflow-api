package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

// Security audit 2026-11-10 (MF #4): every /admin route admitted any service key, and the
// internal key is shared across the platform, so any service could reconfigure providers,
// rotate settings or read the tenant directory. Service keys now reach only the provider
// resolve read that marketflow-ai and the worker use; the rest needs a platform owner.
func TestPlatformOwnerGate(t *testing.T) {
	gate := platformOwnerGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	call := func(method, path string, c *authclient.Claims) int {
		r := httptest.NewRequest(method, path, nil)
		if c != nil {
			r = r.WithContext(authclient.ContextWithClaims(r.Context(), c))
		}
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, r)
		return w.Code
	}
	svc := &authclient.Claims{IsService: true, ServiceName: "pos-service"}
	po := &authclient.Claims{IsPlatformOwner: true}
	admin := &authclient.Claims{Roles: []string{"superuser"}}

	cases := []struct {
		name   string
		method string
		path   string
		c      *authclient.Claims
		want   int
	}{
		{"service resolves providers", http.MethodGet, "/api/v1/admin/providers/resolve?type=meta_ads&name=oauth", svc, 200},
		{"service cannot configure", http.MethodPost, "/api/v1/admin/providers/configure", svc, 403},
		{"service cannot list tenants", http.MethodGet, "/api/v1/admin/tenants", svc, 403},
		{"service cannot write service config", http.MethodPut, "/api/v1/admin/service-config/x", svc, 403},
		{"service cannot post to resolve", http.MethodPost, "/api/v1/admin/providers/resolve", svc, 403},
		{"platform owner configures", http.MethodPost, "/api/v1/admin/providers/configure", po, 200},
		{"tenant superuser refused", http.MethodGet, "/api/v1/admin/providers/resolve", admin, 403},
		{"anonymous refused", http.MethodGet, "/api/v1/admin/tenants", nil, 401},
	}
	for _, c := range cases {
		if got := call(c.method, c.path, c.c); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}
