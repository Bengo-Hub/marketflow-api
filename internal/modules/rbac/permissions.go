package rbac

import (
	"net/http"
	"strings"
)

// Permission is one `resource:action` code of MarketFlow's permission catalogue.
type Permission struct {
	Code, Module, Action, Name, Description string
}

// module is one area of MarketFlow: its permission prefix, label, and the route prefixes (after
// /api/v1) it owns. A test over the router keeps every signed-in tenant route in a module
// (httpapi TestEveryTenantRouteHasAPermission), so a new area cannot ship without one.
type module struct {
	code, label string
	paths       []string
	// adminWrites: every write in the area is an admin's (see adminWritePrefixes).
	adminWrites bool
	// platform: the platform console, held by no tenant role.
	platform bool
}

var modules = []module{
	{code: "leads", label: "Lead", paths: []string{"/leads"}},
	{code: "contacts", label: "Contact", paths: []string{"/contacts"}},
	{code: "knowledge", label: "Knowledge Base", paths: []string{"/knowledge"}},
	{code: "campaigns", label: "Campaign", paths: []string{"/campaigns"}},
	{code: "funnels", label: "Funnel", paths: []string{"/funnels"}},
	{code: "shortlinks", label: "Short Link", paths: []string{"/short-links"}},
	{code: "analytics", label: "Analytics", paths: []string{"/analytics"}},
	{code: "chat", label: "AI Chat"}, // served by marketflow-ai
	{code: "sequences", label: "Sequence", paths: []string{"/sequences"}},
	{code: "meetings", label: "Meeting", paths: []string{"/meetings"}},
	{code: "deals", label: "Deal", paths: []string{"/deals", "/pipelines"}},
	{code: "accounts", label: "Account", paths: []string{"/accounts"}},
	{code: "activities", label: "Activity", paths: []string{"/activities"}},
	{code: "tasks", label: "Task", paths: []string{"/tasks"}},
	{code: "segments", label: "Segment", paths: []string{"/segments"}},
	{code: "custom_fields", label: "Custom Field", paths: []string{"/custom-fields"}},
	{code: "creative", label: "Studio Ad", paths: []string{"/creative"}},
	{code: "publishing", label: "Post", paths: []string{"/publishing"}},
	{code: "inbox", label: "Inbox", paths: []string{"/inbox"}},
	// Boosts and Run ads.
	{code: "ads", label: "Ad", paths: []string{"/ads"}},
	{code: "ads_setup", label: "Ads Setup", paths: []string{"/ads-setup"}, adminWrites: true},
	{code: "testimonials", label: "Testimonial", paths: []string{"/testimonials"}},
	{code: "brand_images", label: "Brand Image and Screenshot", paths: []string{"/tenant/brand-images"}},
	{code: "brand_faces", label: "Brand Face", paths: []string{"/tenant/brand-faces"}, adminWrites: true},
	{code: "market_research", label: "Market Research", paths: []string{"/tenant/market-research"}},
	{code: "agents", label: "Agent", paths: []string{"/agents", "/suggestions"}},
	{code: "team", label: "Team Member", paths: []string{"/tenant/members"}, adminWrites: true},
	{code: "integrations", label: "Integration", paths: []string{"/integrations"}, adminWrites: true},
	{code: "onboarding", label: "Setup Guide", paths: []string{"/tenant/onboarding"}},
	// Profile, currency, lead filter, lead response, Run ads defaults, brand context.
	{code: "settings", label: "Business Setting", adminWrites: true,
		paths: []string{"/tenant/profile", "/tenant/os-settings", "/tenant/marketing-context"}},
	{code: "providers", label: "Provider Config", paths: []string{"/admin/providers"}, platform: true},
	{code: "config", label: "Service Config", paths: []string{"/admin"}, platform: true},
}

var actions = []struct{ code, name, desc string }{
	{"add", "Add", "Create new"},
	{"view", "View", "View all"},
	{"view_own", "View Own", "View own"},
	{"change", "Change", "Edit any"},
	{"change_own", "Change Own", "Edit own"},
	{"delete", "Delete", "Delete"},
	{"manage", "Manage", "Full management"},
}

// adminDeletes are the modules whose record deletes the gate keeps to admins (recordDelete).
var adminDeletes = map[string]bool{"leads": true, "contacts": true}

// Permissions is the whole catalogue, in a stable order.
func Permissions() []Permission {
	out := make([]Permission, 0, len(modules)*len(actions))
	for _, m := range modules {
		for _, a := range actions {
			out = append(out, Permission{
				Code: m.code + ":" + a.code, Module: m.code, Action: a.code,
				Name:        a.name + " " + m.label,
				Description: a.desc + " " + strings.ToLower(m.label) + " records",
			})
		}
	}
	return out
}

// RoleGrants reports whether a system role holds a permission: what the write gate enforces, said
// in permission codes. The app reviewer holds none (its allow-list decides).
func RoleGrants(role, code string) bool {
	mod, action, _ := strings.Cut(code, ":")
	for _, m := range modules {
		if m.code != mod || m.platform {
			continue
		}
		view := strings.HasPrefix(action, "view")
		switch role {
		case RoleTenantAdmin:
			return true
		case RoleMember:
			return view || !(m.adminWrites || action == "manage" || (action == "delete" && adminDeletes[mod]))
		case RoleViewer:
			return view
		}
	}
	return false
}

// PermissionFor is the permission a signed-in tenant request needs, or "" for a path no module
// owns. An admin-only request needs the module's ":manage".
func PermissionFor(method, path string) string {
	p := gatePath(path)
	best, bestLen := "", -1
	for _, m := range modules {
		for _, pre := range m.paths {
			if (p == pre || strings.HasPrefix(p, pre+"/")) && len(pre) > bestLen {
				best, bestLen = m.code, len(pre)
			}
		}
	}
	if smsSendPath.MatchString(p) {
		best = "campaigns"
	}
	if best == "" {
		return ""
	}
	if len(requiredRoles(method, path)) == 1 {
		return best + ":manage"
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return best + ":view"
	case http.MethodPost:
		return best + ":add"
	case http.MethodDelete:
		return best + ":delete"
	}
	return best + ":change"
}

// GateAllows reports whether the write gate lets a system role make a request.
func GateAllows(role, method, path string) bool {
	if restrictedRole([]string{role}) != "" {
		return restrictedAllows([]string{role}, method, path)
	}
	need := requiredRoles(method, path)
	return need == nil || hasAnyRole([]string{role}, need)
}
