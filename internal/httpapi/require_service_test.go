package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

// Security audit 2026-11-10 (MF #6): the notifications delivery webhook sat in the public route
// group with no check, so anyone could post funnel events for any tenant. It now needs a
// service key.
func TestRequireServiceCaller(t *testing.T) {
	h := requireServiceCaller(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	call := func(c *authclient.Claims) int {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if c != nil {
			r = r.WithContext(authclient.ContextWithClaims(r.Context(), c))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := call(&authclient.Claims{IsService: true, ServiceName: "notifications-service"}); got != http.StatusNoContent {
		t.Errorf("service key: %d", got)
	}
	if got := call(&authclient.Claims{Roles: []string{"admin"}}); got != http.StatusForbidden {
		t.Errorf("signed-in user: %d", got)
	}
	if got := call(nil); got != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", got)
	}
}
