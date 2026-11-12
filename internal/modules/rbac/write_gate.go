package rbac

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Owner decision 2026-11-10 (audit MF #5): viewers read only; members do day-to-day marketing
// (create, approve, boost, post, reply, spend credits); admins own settings, integrations, team,
// autopilot and lead export or delete.
var (
	memberOrAdmin = []string{RoleTenantAdmin, RoleMember}
	adminOnly     = []string{RoleTenantAdmin}
)

// adminWritePrefixes are the areas only a MarketFlow admin may change.
var adminWritePrefixes = []string{
	"/tenant/profile", "/tenant/members", "/tenant/os-settings", "/tenant/marketing-context",
	"/integrations", "/agents/team", "/custom-fields/defs",
}

var (
	autopilotPath = regexp.MustCompile(`^/agents/custom/[^/]+/autopilot(/|$)`)
	recordDelete  = regexp.MustCompile(`^/(leads|contacts)/[^/]+$`)
	exportPath    = regexp.MustCompile(`^/(leads|contacts)/export$`)
	// connectPath starts an integration's OAuth connection. It is a GET, yet it connects or
	// replaces the business's account, so owner 2026-11-12 made it admin only.
	connectPath = regexp.MustCompile(`^/(integrations/[^/]+|meta)/authorize$`)
)

// anyRoleWrites are writes every signed-in member may make (their own UI state).
var anyRoleWrites = map[string]bool{"/tenant/onboarding/dismiss": true}

// requiredRoles is the role a request needs, or nil when any signed-in member may make it.
func requiredRoles(method, path string) []string {
	p := strings.TrimPrefix(path, "/api/v1")
	switch method {
	case http.MethodGet, http.MethodHead:
		if exportPath.MatchString(p) || connectPath.MatchString(p) {
			return adminOnly
		}
		return nil
	case http.MethodOptions:
		return nil
	}
	if anyRoleWrites[p] {
		return nil
	}
	for _, pre := range adminWritePrefixes {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return adminOnly
		}
	}
	if autopilotPath.MatchString(p) {
		return adminOnly
	}
	if method == http.MethodDelete && recordDelete.MatchString(p) {
		return adminOnly
	}
	return memberOrAdmin
}

// WriteGate is MarketFlow's role gate for the tenant the request acts for (X-Tenant-ID, set by
// the tenant middleware from the token). A caller whose effective role is a restricted one (the
// app reviewer) may make only the requests on its allow-list, reads and writes alike
// (restrictedRoleCapabilities). Everyone else needs requiredRoles. Platform owners and service
// keys pass. Run it after SyncMiddleware so the caller's role is current.
func (s *Service) WriteGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		tid, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
		if err != nil {
			// No tenant to check against: let the handler answer (it rejects a missing tenant).
			next.ServeHTTP(w, r)
			return
		}
		caller := s.callerAccess(r.Context(), tid)
		if caller.bypass {
			next.ServeHTTP(w, r)
			return
		}
		if restrictedRole(caller.roles) != "" {
			if !restrictedAllows(caller.roles, r.Method, r.URL.Path) {
				httpapi.Error(w, http.StatusForbidden, "not_available_for_role",
					"this account cannot use this part of MarketFlow", nil)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		need := requiredRoles(r.Method, r.URL.Path)
		if need == nil {
			next.ServeHTTP(w, r)
			return
		}
		if !hasAnyRole(caller.roles, need) {
			msg := "your MarketFlow role can view but not change this; ask a MarketFlow admin"
			if len(need) == 1 {
				msg = "only a MarketFlow admin can change this"
			}
			httpapi.Error(w, http.StatusForbidden, "role_required", msg, nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
