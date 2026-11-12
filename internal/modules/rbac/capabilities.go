package rbac

import (
	"net/http"
	"regexp"
	"strings"
)

// RoleAppReviewer is the Meta App Review account's MarketFlow role, mapped from the SSO role
// marketflow_reviewer. Owner 2026-11-12: it may do exactly what Meta's reviewers must test and
// nothing else, so its access is the allow-list below instead of the role ladder.
const RoleAppReviewer = "app_reviewer"

// roleRank orders the MarketFlow system roles. When a caller holds several, the highest decides:
// a reviewer who is also a member or an admin is treated as the member or admin.
var roleRank = map[string]int{RoleViewer: 0, RoleAppReviewer: 1, RoleMember: 2, RoleTenantAdmin: 3}

// effectiveRole is the highest ranked system role in roles, or "" when none is known.
func effectiveRole(roles []string) string {
	best, bestRank := "", -1
	for _, r := range roles {
		if rank, ok := roleRank[r]; ok && rank > bestRank {
			best, bestRank = r, rank
		}
	}
	return best
}

// Capability lets a restricted role make one kind of request: one of Methods on a path matching
// Path. Path is a route template after /api/v1 in which {name} stands for one path segment.
type Capability struct {
	Methods []string
	Path    string
}

// readable is a GET capability.
func readable(path string) Capability {
	return Capability{Methods: []string{http.MethodGet}, Path: path}
}

// postable is a POST capability.
func postable(path string) Capability {
	return Capability{Methods: []string{http.MethodPost}, Path: path}
}

// restrictedRoleCapabilities are the roles whose access is an explicit allow-list for reads and
// writes alike. Anything not listed is refused, so a route added later stays closed to them until
// it is added here on purpose. A future restricted role is one more entry (plus its place in
// roleRank and systemRoleDefs).
var restrictedRoleCapabilities = map[string][]Capability{
	RoleAppReviewer: appReviewerCapabilities,
}

// appReviewerCapabilities are the steps Meta's reviewers test: connect Facebook and Instagram,
// boost, post, reply, view leads and results.
var appReviewerCapabilities = []Capability{
	// App bootstrap: who am I, the business, the setup checklist (dismissing it is the caller's own UI state).
	readable("/me/access"),
	readable("/tenant/profile"),
	readable("/tenant/onboarding"),
	postable("/tenant/onboarding/dismiss"),

	// Dashboard.
	readable("/analytics/dashboard"),
	readable("/analytics/profile"),

	// Studio, read only: the ads, one ad, one version.
	readable("/creative/briefs"),
	readable("/creative/briefs/{id}"),
	readable("/creative/versions/{id}"),

	// Boosts: the ad accounts and quote the Boost dialog needs, launch, confirm the budget on Meta,
	// pause and resume; the results show on the campaign pages.
	readable("/ads/accounts"),
	postable("/ads/quote"),
	postable("/ads/boosts"),
	readable("/ads/boosts/{id}"),
	postable("/ads/boosts/{id}/paid"),
	postable("/ads/boosts/{id}/pause"),
	postable("/ads/boosts/{id}/resume"),
	readable("/campaigns"),
	readable("/campaigns/{id}"),
	readable("/analytics/campaigns/{id}"),

	// Posting an approved version to the Facebook Page or Instagram, and seeing it go out.
	readable("/publishing/accounts"),
	readable("/publishing/jobs"),
	postable("/publishing/jobs"),

	// Leads from the Page and the ads.
	readable("/leads"),
	readable("/leads/{id}"),

	// Inbox: comments and messages, replying, and hiding or deleting a comment on the business's
	// own post (the item itself stays; only the public comment goes).
	readable("/inbox"),
	postable("/inbox/{id}/reply"),
	postable("/inbox/{id}/hide"),
	{Methods: []string{http.MethodDelete}, Path: "/inbox/{id}"},

	// Integrations: the list, and connecting Meta. The Page, Instagram account and ad account are
	// chosen in Meta's own consent dialog and saved by the public callback.
	readable("/integrations"),
	readable("/integrations/available"),
	readable("/integrations/meta/authorize"),
}

// WithAppReviewer returns roles plus the app reviewer, as a new slice. Handlers that check a role
// themselves for an action on the reviewer's allow-list (launch, pay, pause or resume a boost,
// post, reply) use it; the gate already keeps the reviewer to exactly those routes.
func WithAppReviewer(roles []string) []string {
	out := make([]string, 0, len(roles)+1)
	out = append(out, roles...)
	return append(out, RoleAppReviewer)
}

type compiledCapability struct {
	methods map[string]bool
	path    *regexp.Regexp
}

var compiledCapabilities = compileCapabilities(restrictedRoleCapabilities)

var templateParam = regexp.MustCompile(`^\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// compileCapabilities turns each route template into an anchored pattern; a {name} segment
// matches one UUID (every MarketFlow record id is one), so a fixed word in the same position,
// such as /leads/export, never passes as an id.
func compileCapabilities(table map[string][]Capability) map[string][]compiledCapability {
	out := make(map[string][]compiledCapability, len(table))
	for role, caps := range table {
		for _, c := range caps {
			segs := strings.Split(strings.Trim(c.Path, "/"), "/")
			for i, s := range segs {
				if templateParam.MatchString(s) {
					segs[i] = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
				} else {
					segs[i] = regexp.QuoteMeta(s)
				}
			}
			methods := make(map[string]bool, len(c.Methods))
			for _, m := range c.Methods {
				methods[strings.ToUpper(m)] = true
			}
			out[role] = append(out[role], compiledCapability{
				methods: methods,
				path:    regexp.MustCompile(`^/` + strings.Join(segs, "/") + `$`),
			})
		}
	}
	return out
}

// restrictedRole is the caller's effective role when that role is a restricted one, else "".
func restrictedRole(roles []string) string {
	role := effectiveRole(roles)
	if _, ok := restrictedRoleCapabilities[role]; ok {
		return role
	}
	return ""
}

// restrictedAllows reports whether a caller with a restricted effective role may make the request.
// HEAD is a GET; OPTIONS (CORS preflight) always passes.
func restrictedAllows(roles []string, method, path string) bool {
	role := restrictedRole(roles)
	if role == "" {
		return false
	}
	method = strings.ToUpper(method)
	switch method {
	case http.MethodOptions:
		return true
	case http.MethodHead:
		method = http.MethodGet
	}
	p := gatePath(path)
	for _, c := range compiledCapabilities[role] {
		if c.methods[method] && c.path.MatchString(p) {
			return true
		}
	}
	return false
}

// gatePath is the request path as the gates compare it: without /api/v1 and a trailing slash.
func gatePath(path string) string {
	p := strings.TrimPrefix(path, "/api/v1")
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		p = "/"
	}
	return p
}
