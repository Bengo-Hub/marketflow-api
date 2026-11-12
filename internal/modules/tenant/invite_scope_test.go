package tenant

import (
	"testing"

	"github.com/bengobox/marketflow-api/internal/modules/rbac"
)

// Security audit 2026-11-10 (MF #3), owner decision: a MarketFlow invite grants MarketFlow-only
// SSO roles, never the organisation-wide admin (admin of POS, Treasury and every other app).
func TestInviteRolesAreMarketflowScoped(t *testing.T) {
	want := map[string][]string{
		rbac.RoleTenantAdmin: {"marketflow_admin"},
		rbac.RoleMember:      {"marketflow_member"},
		rbac.RoleViewer:      {"member"},
		rbac.RoleAppReviewer: {"marketflow_reviewer"},
	}
	for local, roles := range want {
		got := inviteRoles[local]
		if len(got) != len(roles) || got[0] != roles[0] {
			t.Errorf("%s: got %v want %v", local, got, roles)
		}
	}
	for _, roles := range inviteRoles {
		for _, r := range roles {
			if r == "admin" || r == "superuser" || r == "owner" {
				t.Fatalf("an invite must never grant the organisation-wide %q", r)
			}
		}
	}
}

// MF #3: the invite answer never carries the new person's user id or a temporary password; a new
// account signs in from the emailed link.
func TestInviteResponseLeaksNothing(t *testing.T) {
	got := inviteResponse("a@b.c", rbac.RoleMember, true)
	for _, k := range []string{"user_id", "temp_password"} {
		if _, ok := got[k]; ok {
			t.Errorf("invite response has %s", k)
		}
	}
	if got["new_account"] != true || got["email"] != "a@b.c" || got["role"] != rbac.RoleMember {
		t.Errorf("response: %v", got)
	}
}
