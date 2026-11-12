package rbac

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Security audit 2026-11-10 (MF #5), owner decision: viewers read only; members do day-to-day
// marketing; admins own settings, integrations, team, autopilot and lead export or delete.
func TestRequiredRoles(t *testing.T) {
	member := []string{RoleTenantAdmin, RoleMember}
	admin := []string{RoleTenantAdmin}
	cases := []struct {
		method, path string
		want         []string
	}{
		{"GET", "/api/v1/leads", nil},
		{"GET", "/api/v1/leads/export", admin},
		{"POST", "/api/v1/leads", member},
		{"DELETE", "/api/v1/leads/123", admin},
		{"DELETE", "/api/v1/contacts/123", admin},
		{"POST", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/approve", member},
		{"POST", "/api/v1/ads/boosts/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/paid", member},
		{"POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/reply", member},
		{"POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/hide", member},
		{"DELETE", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", member},
		{"PUT", "/api/v1/tenant/os-settings", admin},
		{"PUT", "/api/v1/tenant/marketing-context", admin},
		{"PUT", "/api/v1/tenant/profile", admin},
		{"PATCH", "/api/v1/tenant/profile", admin},
		{"POST", "/api/v1/tenant/members/invite", admin},
		{"DELETE", "/api/v1/integrations/meta_ads", admin},
		{"PUT", "/api/v1/agents/team/vera", admin},
		{"POST", "/api/v1/agents/team/setup", admin},
		{"PUT", "/api/v1/agents/custom/9/autopilot", admin},
		{"POST", "/api/v1/agents/custom/9/autopilot/rollback", admin},
		{"POST", "/api/v1/custom-fields/defs", admin},
		{"POST", "/api/v1/short-links", member},
		{"POST", "/api/v1/tenant/onboarding/dismiss", nil},
		{"OPTIONS", "/api/v1/leads", nil},
		// Owner 2026-11-12: starting a connection is admin only, even though it is a GET.
		{"GET", "/api/v1/integrations/meta/authorize", admin},
		{"GET", "/api/v1/integrations/google/authorize", admin},
		{"GET", "/api/v1/integrations/tiktok/authorize", admin},
		{"GET", "/api/v1/integrations/google-ads/authorize", admin},
		{"GET", "/api/v1/meta/authorize", admin},
		{"GET", "/api/v1/integrations", nil},
		{"GET", "/api/v1/integrations/available", nil},
	}
	for _, c := range cases {
		got := requiredRoles(c.method, c.path)
		if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) || (len(got) == 2) != (len(c.want) == 2) {
			t.Errorf("%s %s: got %v want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestWriteGate(t *testing.T) {
	repo := &memRepo{}
	s := NewService(repo, zap.NewNop())
	tenant := uuid.New()
	viewer, member := uuid.New(), uuid.New()
	_ = s.SyncCaller(t.Context(), tenant, viewer, "v@x", []string{"member"})
	_ = s.SyncCaller(t.Context(), tenant, member, "m@x", []string{"marketflow_member"})

	gate := s.WriteGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	call := func(method, path string, who uuid.UUID) int {
		c := &authclient.Claims{TenantID: tenant.String()}
		c.Subject = who.String()
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("X-Tenant-ID", tenant.String())
		r = r.WithContext(authclient.ContextWithClaims(r.Context(), c))
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, r)
		return w.Code
	}
	if got := call("GET", "/api/v1/leads", viewer); got != 200 {
		t.Errorf("viewer reads: %d", got)
	}
	if got := call("POST", "/api/v1/leads", viewer); got != 403 {
		t.Errorf("viewer writes: %d", got)
	}
	if got := call("POST", "/api/v1/ads/boosts/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/paid", member); got != 200 {
		t.Errorf("member boosts: %d", got)
	}
	if got := call("DELETE", "/api/v1/integrations/meta_ads", member); got != 403 {
		t.Errorf("member disconnects Meta: %d", got)
	}
}

// Owner 2026-11-12: the app reviewer gets its allow-list for reads and writes alike; connecting an
// integration is admin only (plus the reviewer, through its list); viewers and members are as
// before otherwise.
func TestGateAppReviewer(t *testing.T) {
	repo := &memRepo{}
	s := NewService(repo, zap.NewNop())
	tenant := uuid.New()
	viewer, member, admin, reviewer, reviewerAndMember, unsynced := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	_ = s.SyncCaller(t.Context(), tenant, viewer, "v@x", []string{"member"})
	_ = s.SyncCaller(t.Context(), tenant, member, "m@x", []string{"marketflow_member"})
	_ = s.SyncCaller(t.Context(), tenant, admin, "a@x", []string{"marketflow_admin"})
	_ = s.SyncCaller(t.Context(), tenant, reviewer, "r@x", []string{"member", "marketflow_reviewer"})
	_ = s.SyncCaller(t.Context(), tenant, reviewerAndMember, "rm@x", []string{"marketflow_reviewer", "marketflow_member"})

	gate := s.WriteGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	call := func(method, path string, who uuid.UUID, platformOwner bool) int {
		c := &authclient.Claims{TenantID: tenant.String(), IsPlatformOwner: platformOwner}
		c.Subject = who.String()
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("X-Tenant-ID", tenant.String())
		r = r.WithContext(authclient.ContextWithClaims(r.Context(), c))
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, r)
		return w.Code
	}
	cases := []struct {
		name         string
		method, path string
		who          uuid.UUID
		want         int
	}{
		{"reviewer reads leads", "GET", "/api/v1/leads", reviewer, 200},
		{"reviewer reads a lead", "GET", "/api/v1/leads/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", reviewer, 200},
		{"reviewer boosts", "POST", "/api/v1/ads/boosts", reviewer, 200},
		{"reviewer pauses a boost", "POST", "/api/v1/ads/boosts/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/pause", reviewer, 200},
		{"reviewer posts", "POST", "/api/v1/publishing/jobs", reviewer, 200},
		{"reviewer replies", "POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/reply", reviewer, 200},
		{"reviewer hides a comment", "POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/hide", reviewer, 200},
		{"reviewer deletes a comment", "DELETE", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", reviewer, 200},
		{"viewer cannot hide a comment", "POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/hide", viewer, 403},
		{"viewer cannot delete a comment", "DELETE", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", viewer, 403},
		{"member hides a comment", "POST", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/hide", member, 200},
		{"member deletes a comment", "DELETE", "/api/v1/inbox/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", member, 200},
		{"reviewer connects Meta", "GET", "/api/v1/integrations/meta/authorize", reviewer, 200},
		{"reviewer cannot list the team", "GET", "/api/v1/tenant/members", reviewer, 403},
		{"reviewer cannot invite", "POST", "/api/v1/tenant/members/invite", reviewer, 403},
		{"reviewer cannot read contacts", "GET", "/api/v1/contacts", reviewer, 403},
		{"reviewer cannot read the audit log", "GET", "/api/v1/analytics/audit", reviewer, 403},
		{"reviewer cannot export", "GET", "/api/v1/leads/export", reviewer, 403},
		{"reviewer cannot delete", "DELETE", "/api/v1/leads/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b", reviewer, 403},
		{"reviewer cannot generate", "POST", "/api/v1/creative/briefs", reviewer, 403},
		{"reviewer cannot render", "POST", "/api/v1/creative/versions/7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b/render", reviewer, 403},
		{"reviewer cannot change settings", "PUT", "/api/v1/tenant/os-settings", reviewer, 403},
		{"reviewer cannot read agents", "GET", "/api/v1/agents/team", reviewer, 403},
		{"reviewer cannot disconnect", "DELETE", "/api/v1/integrations/meta_ads", reviewer, 403},
		{"reviewer cannot connect Google", "GET", "/api/v1/integrations/google/authorize", reviewer, 403},

		{"a higher role wins over the reviewer", "GET", "/api/v1/contacts", reviewerAndMember, 200},
		{"a higher role wins: member writes", "POST", "/api/v1/creative/briefs", reviewerAndMember, 200},

		{"admin connects Meta", "GET", "/api/v1/integrations/meta/authorize", admin, 200},
		{"admin connects Google", "GET", "/api/v1/integrations/google/authorize", admin, 200},
		{"member cannot connect Meta", "GET", "/api/v1/integrations/meta/authorize", member, 403},
		{"viewer cannot connect Meta", "GET", "/api/v1/integrations/meta/authorize", viewer, 403},
		{"member still reads integrations", "GET", "/api/v1/integrations", member, 200},

		{"viewer still reads contacts", "GET", "/api/v1/contacts", viewer, 200},
		{"viewer still reads the team", "GET", "/api/v1/tenant/members", viewer, 200},
		{"viewer still cannot write", "POST", "/api/v1/ads/boosts", viewer, 403},
		{"member still generates", "POST", "/api/v1/creative/briefs", member, 200},
		{"member still cannot export", "GET", "/api/v1/leads/export", member, 403},
		{"an unsynced caller still reads", "GET", "/api/v1/leads", unsynced, 200},
		{"an unsynced caller still cannot write", "POST", "/api/v1/leads", unsynced, 403},
	}
	for _, c := range cases {
		if got := call(c.method, c.path, c.who, false); got != c.want {
			t.Errorf("%s: %s %s = %d, want %d", c.name, c.method, c.path, got, c.want)
		}
	}
	if got := call("GET", "/api/v1/tenant/members", uuid.New(), true); got != 200 {
		t.Errorf("a platform owner passes: %d", got)
	}

	// A reviewer whose local user is not synced yet is still held to the allow-list by its token.
	unsyncedReviewer := func(path string) int {
		c := &authclient.Claims{TenantID: tenant.String(), Roles: []string{"member", "marketflow_reviewer"}}
		c.Subject = uuid.NewString()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Tenant-ID", tenant.String())
		r = r.WithContext(authclient.ContextWithClaims(r.Context(), c))
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, r)
		return w.Code
	}
	if got := unsyncedReviewer("/api/v1/contacts"); got != 403 {
		t.Errorf("unsynced reviewer reads contacts: %d", got)
	}
	if got := unsyncedReviewer("/api/v1/leads"); got != 200 {
		t.Errorf("unsynced reviewer reads leads: %d", got)
	}
}
