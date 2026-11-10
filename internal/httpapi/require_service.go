package httpapi

import (
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

// requireServiceCaller admits only service-key callers (internal peers such as
// notifications-api). It runs after the auth middleware has validated the key.
func requireServiceCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authclient.ClaimsFromContext(r.Context())
		if !ok || c == nil {
			Error(w, http.StatusUnauthorized, "unauthorized", "service key required", nil)
			return
		}
		if !c.IsService {
			Error(w, http.StatusForbidden, "forbidden", "service key required", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
