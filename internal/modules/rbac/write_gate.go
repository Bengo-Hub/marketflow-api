package rbac

import (
	"net/http"
	"regexp"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"
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
	// The Autopilot terms and the veto-window hold categories are autopilot settings.
	"/agents/autopilot/auto-publish",
}

var (
	autopilotPath = regexp.MustCompile(`^/agents/custom/[^/]+/autopilot(/|$)`)
	// A lead, a contact or a Studio ad deleted for good (the ad: owner 2026-11-19).
	recordDelete = regexp.MustCompile(`^/(leads|contacts|creative/briefs)/[^/]+$|^/funnels/[^/]+/permanent$`)
	exportPath    = regexp.MustCompile(`^/(leads|contacts)/export$`)
	// connectPath starts an integration's OAuth connection. It is a GET, yet it connects or
	// replaces the business's account, so owner 2026-11-12 made it admin only.
	connectPath = regexp.MustCompile(`^/(integrations/[^/]+|meta)/authorize$`)
)

// spendPaths are the writes that spend the business's money or post in its name: launching or
// restarting a boost (and its confirmation code), every publishing write, and sending an SMS
// campaign. A service key must name the person it acts for (X-Acting-User-ID) to make them.
var spendPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/ads/boosts(/otp)?$`),
	regexp.MustCompile(`^/ads/boosts/[^/]+/(paid|resume)$`),
	regexp.MustCompile(`^/ads/boosts/[^/]+/ads/[^/]+/resume$`),
	regexp.MustCompile(`^/publishing/jobs(/[^/]+/[^/]+)?$`),
	smsSendPath,
}

// smsSendPath is POST /{tenantID}/campaigns/{campaignID}/send-sms; group 1 is the tenant.
var smsSendPath = regexp.MustCompile(`^/([^/]+)/campaigns/[^/]+/send-sms$`)

func spendOrPublish(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	p := gatePath(path)
	for _, re := range spendPaths {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// serviceOrigins are X-Acting-Origin values that may make spendPaths requests with a service key
// and no acting user. Empty: no internal caller makes these requests over HTTP (checked
// 2026-11-16 across marketflow-api, marketflow-ai and the other platform services); the worker
// calls the module services directly.
var serviceOrigins = map[string]bool{}

func serviceOriginAllowed(origin string) bool {
	return origin != "" && serviceOrigins[origin]
}

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
// (restrictedRoleCapabilities). Everyone else needs requiredRoles. Platform owners pass. A service
// key passes unless it names a person in X-Acting-User-ID, who is then held to their own local
// role in the tenant, and it may not make a spendPaths request without one. Run it after
// SyncMiddleware so the caller's role is current.
func (s *Service) WriteGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		spend := spendOrPublish(r.Method, r.URL.Path)
		acting := false
		if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil && c.IsService && !c.IsPlatformOwner {
			a := httpapi.ActingFrom(r)
			switch {
			case a.Invalid:
				httpapi.Error(w, http.StatusBadRequest, "invalid_acting_user", "X-Acting-User-ID must be a user id", nil)
				return
			case a.UserID != uuid.Nil:
				r = r.WithContext(httpapi.WithActingUser(r.Context(), a.UserID))
				acting = true
			case spend && !serviceOriginAllowed(a.Origin):
				httpapi.Error(w, http.StatusForbidden, "acting_user_required", "acting_user_required", nil)
				return
			}
		}
		tid, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
		if m := smsSendPath.FindStringSubmatch(gatePath(r.URL.Path)); m != nil {
			// The send-sms handler acts for the tenant in its path, so that is the tenant to check.
			tid, err = uuid.Parse(m[1])
		}
		if err != nil {
			if acting && r.Method != http.MethodGet && r.Method != http.MethodHead {
				// An acting user needs a tenant to hold a role in.
				httpapi.Error(w, http.StatusForbidden, "role_required", "no tenant to check the acting user's role in", nil)
				return
			}
			// No tenant to check against: let the handler answer (it rejects a missing tenant).
			next.ServeHTTP(w, r)
			return
		}
		caller := s.callerAccess(r.Context(), tid)
		if caller.bypass {
			next.ServeHTTP(w, r)
			return
		}
		if acting && spend && !hasAnyRole(caller.roles, memberOrAdmin) {
			// Spending or posting for a person through a service needs a member or admin.
			httpapi.Error(w, http.StatusForbidden, "role_required",
				"your MarketFlow role can view but not change this; ask a MarketFlow admin", nil)
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
