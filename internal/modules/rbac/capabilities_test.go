package rbac

import (
	"net/http"
	"strings"
	"testing"
)

// Owner 2026-11-12: the Meta App Review account (SSO marketflow_reviewer, MarketFlow app_reviewer)
// may do exactly what Meta's reviewers test and nothing else. Its access is an explicit allow-list,
// not the role ladder, so a new route is closed to it until someone adds it here on purpose.
func TestAppReviewerAllowList(t *testing.T) {
	reviewer := []string{RoleAppReviewer}
	cases := []struct {
		method, path string
		want         bool
	}{
		// App bootstrap.
		{"GET", "/api/v1/me/access", true},
		{"GET", "/api/v1/tenant/profile", true},
		{"GET", "/api/v1/tenant/profile/", true},
		{"GET", "/api/v1/tenant/onboarding", true},
		{"POST", "/api/v1/tenant/onboarding/dismiss", true},
		// Dashboard.
		{"GET", "/api/v1/analytics/dashboard", true},
		{"GET", "/api/v1/analytics/profile", true},
		// Studio: lists and a version's detail, read only.
		{"GET", "/api/v1/creative/briefs", true},
		{"GET", "/api/v1/creative/briefs/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", true},
		{"GET", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", true},
		// Boosts and their results.
		{"GET", "/api/v1/campaigns", true},
		{"GET", "/api/v1/campaigns/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", true},
		{"GET", "/api/v1/analytics/campaigns/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", true},
		{"GET", "/api/v1/ads/accounts", true},
		{"GET", "/api/v1/ads/boosts/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", true},
		{"POST", "/api/v1/ads/quote", true},
		{"POST", "/api/v1/ads/boosts", true},
		{"POST", "/api/v1/ads/boosts/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/paid", true},
		{"POST", "/api/v1/ads/boosts/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/pause", true},
		{"POST", "/api/v1/ads/boosts/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/resume", true},
		// Posting an approved version.
		{"GET", "/api/v1/publishing/accounts", true},
		{"GET", "/api/v1/publishing/jobs", true},
		{"POST", "/api/v1/publishing/jobs", true},
		// Leads.
		{"GET", "/api/v1/leads", true},
		{"GET", "/api/v1/leads/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", true},
		// Inbox.
		{"GET", "/api/v1/inbox", true},
		{"POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/reply", true},
		{"POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/hide", true},
		{"DELETE", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", true},
		{"GET", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", false},
		{"DELETE", "/api/v1/inbox", false},
		// Integrations and the Meta connection.
		{"GET", "/api/v1/integrations", true},
		{"GET", "/api/v1/integrations/available", true},
		{"GET", "/api/v1/integrations/meta/authorize", true},
		{"HEAD", "/api/v1/leads", true},
		{"OPTIONS", "/api/v1/contacts", true},

		// Team, members and invites.
		{"GET", "/api/v1/tenant/members", false},
		{"POST", "/api/v1/tenant/members/invite", false},
		{"PUT", "/api/v1/tenant/members/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", false},
		// Audit and activity.
		{"GET", "/api/v1/analytics/audit", false},
		{"GET", "/api/v1/activities", false},
		{"GET", "/api/v1/leads/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/timeline", false},
		// Agents and autopilot.
		{"GET", "/api/v1/agents/team", false},
		{"GET", "/api/v1/agents/custom", false},
		{"PUT", "/api/v1/agents/custom/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/autopilot", false},
		{"POST", "/api/v1/agents/custom/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/run", false},
		// AI generation.
		{"POST", "/api/v1/creative/briefs", false},
		{"PUT", "/api/v1/creative/briefs/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", false},
		{"POST", "/api/v1/creative/briefs/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/versions", false},
		{"POST", "/api/v1/creative/briefs/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/video-ads", false},
		{"POST", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/approve", false},
		{"POST", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/storyboard", false},
		{"POST", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/render", false},
		{"POST", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/revise", false},
		{"POST", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/motion", false},
		{"POST", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/extend", false},
		{"GET", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/quote", false},
		{"POST", "/api/v1/tenant/market-research/complaints", false},
		// Contacts and CRM.
		{"GET", "/api/v1/contacts", false},
		{"GET", "/api/v1/contacts/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", false},
		{"GET", "/api/v1/deals", false},
		{"GET", "/api/v1/segments", false},
		{"POST", "/api/v1/leads", false},
		{"PUT", "/api/v1/leads/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", false},
		{"POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/promote", false},
		{"POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/handled", false},
		// Export and deletes.
		{"GET", "/api/v1/leads/export", false},
		{"GET", "/api/v1/contacts/export", false},
		{"DELETE", "/api/v1/leads/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", false},
		{"DELETE", "/api/v1/integrations/meta_ads", false},
		{"DELETE", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/review-link", false},
		// Tenant settings writes, custom fields, bulk import, SMS.
		{"PATCH", "/api/v1/tenant/profile", false},
		{"PUT", "/api/v1/tenant/os-settings", false},
		{"GET", "/api/v1/tenant/os-settings", false},
		{"PUT", "/api/v1/tenant/marketing-context", false},
		{"GET", "/api/v1/custom-fields/defs", false},
		{"GET", "/api/v1/leads/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/custom-fields", false},
		{"POST", "/api/v1/contacts/bulk-import", false},
		{"POST", "/api/v1/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/campaigns/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/send-sms", false},
		// Campaign writes outside a boost, other connections, the rest of the ads area.
		{"POST", "/api/v1/campaigns", false},
		{"POST", "/api/v1/campaigns/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/pause", false},
		{"POST", "/api/v1/ads/accounts/kes", false},
		{"POST", "/api/v1/ads/boosts/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/ads/9/resume", false},
		{"GET", "/api/v1/integrations/google/authorize", false},
		{"GET", "/api/v1/integrations/tiktok/authorize", false},
		{"POST", "/api/v1/publishing/jobs/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/cancel", false},
		// Platform console.
		{"GET", "/api/v1/admin/tenants", false},
		// A segment more or a different method never matches.
		{"GET", "/api/v1/leads/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/extra", false},
		{"DELETE", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/reply", false},
		{"PUT", "/api/v1/ads/boosts", false},
	}
	for _, c := range cases {
		if got := restrictedAllows(reviewer, c.method, c.path); got != c.want {
			t.Errorf("%s %s: allowed %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// The ladder still decides when the caller holds a role above the reviewer: the higher role wins.
func TestRestrictedRoleIsTheHighestRole(t *testing.T) {
	cases := []struct {
		roles []string
		want  bool
	}{
		{[]string{RoleAppReviewer}, true},
		{[]string{RoleViewer, RoleAppReviewer}, true},
		{[]string{RoleAppReviewer, RoleMember}, false},
		{[]string{RoleTenantAdmin, RoleAppReviewer}, false},
		{[]string{RoleViewer}, false},
		{[]string{RoleMember}, false},
		{nil, false},
		{[]string{"something_custom"}, false},
	}
	for _, c := range cases {
		if got := restrictedRole(c.roles) != ""; got != c.want {
			t.Errorf("%v: restricted %v, want %v", c.roles, got, c.want)
		}
	}
	if got := effectiveRole([]string{RoleViewer, RoleAppReviewer}); got != RoleAppReviewer {
		t.Errorf("reviewer outranks viewer, got %s", got)
	}
	if got := effectiveRole([]string{RoleAppReviewer, RoleMember}); got != RoleMember {
		t.Errorf("member outranks reviewer, got %s", got)
	}
}

// The handler checks for boost, post and reply let the reviewer through without changing the
// shared approval roles (approving an ad stays with members and admins).
func TestWithAppReviewer(t *testing.T) {
	base := []string{RoleTenantAdmin, RoleMember}
	got := WithAppReviewer(base)
	if len(got) != 3 || got[2] != RoleAppReviewer {
		t.Fatalf("got %v", got)
	}
	if len(base) != 2 {
		t.Fatalf("input changed: %v", base)
	}
}

// Every capability is well formed, so a typo cannot silently open or close a route.
func TestCapabilityTableIsWellFormed(t *testing.T) {
	known := map[string]bool{http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
		http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true}
	if len(restrictedRoleCapabilities) == 0 {
		t.Fatal("no restricted roles")
	}
	for role, caps := range restrictedRoleCapabilities {
		if _, ok := roleRank[role]; !ok {
			t.Errorf("%s has no place in the role ladder", role)
		}
		for _, c := range caps {
			if len(c.Methods) == 0 || !strings.HasPrefix(c.Path, "/") || strings.HasPrefix(c.Path, "/api/v1") {
				t.Errorf("%s: bad capability %+v", role, c)
			}
			for _, m := range c.Methods {
				if !known[m] {
					t.Errorf("%s: unknown method %q in %s", role, m, c.Path)
				}
			}
		}
	}
}
