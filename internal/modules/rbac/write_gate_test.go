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
		{"POST", "/api/v1/creative/versions/1/approve", member},
		{"POST", "/api/v1/ads/boosts/1/paid", member},
		{"POST", "/api/v1/inbox/1/reply", member},
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
	if got := call("POST", "/api/v1/ads/boosts/1/paid", member); got != 200 {
		t.Errorf("member boosts: %d", got)
	}
	if got := call("DELETE", "/api/v1/integrations/meta_ads", member); got != 403 {
		t.Errorf("member disconnects Meta: %d", got)
	}
}
