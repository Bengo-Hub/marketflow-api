package rbac

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The UI shapes its menu and buttons from GET /api/v1/me/access: the caller's effective MarketFlow
// role and whether it is a restricted (allow-list) role. The reviewer cannot read the team list,
// so the UI must not depend on it.
func TestMyAccess(t *testing.T) {
	repo := &memRepo{}
	s := NewService(repo, zap.NewNop())
	tenant := uuid.New()
	admin, reviewer, unsynced := uuid.New(), uuid.New(), uuid.New()
	_ = s.SyncCaller(t.Context(), tenant, admin, "a@x", []string{"admin"})
	_ = s.SyncCaller(t.Context(), tenant, reviewer, "r@x", []string{"member", "marketflow_reviewer"})

	type body struct {
		Role          string   `json:"role"`
		Roles         []string `json:"roles"`
		Restricted    bool     `json:"restricted"`
		PlatformOwner bool     `json:"platform_owner"`
	}
	get := func(who uuid.UUID, platformOwner bool, tenantHeader string) (int, body) {
		c := &authclient.Claims{TenantID: tenant.String(), IsPlatformOwner: platformOwner}
		c.Subject = who.String()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me/access", nil)
		r.Header.Set("X-Tenant-ID", tenantHeader)
		r = r.WithContext(authclient.ContextWithClaims(r.Context(), c))
		w := httptest.NewRecorder()
		s.MyAccess(w, r)
		var b body
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		return w.Code, b
	}

	if code, b := get(reviewer, false, tenant.String()); code != 200 || b.Role != RoleAppReviewer || !b.Restricted || b.PlatformOwner {
		t.Errorf("reviewer: %d %+v", code, b)
	}
	if code, b := get(admin, false, tenant.String()); code != 200 || b.Role != RoleTenantAdmin || b.Restricted {
		t.Errorf("admin: %d %+v", code, b)
	}
	if code, b := get(uuid.New(), true, tenant.String()); code != 200 || !b.PlatformOwner || b.Restricted {
		t.Errorf("platform owner: %d %+v", code, b)
	}
	if code, b := get(unsynced, false, tenant.String()); code != 200 || b.Role != "" || b.Restricted || len(b.Roles) != 0 {
		t.Errorf("unsynced caller: %d %+v", code, b)
	}
	if code, _ := get(admin, false, "not-a-uuid"); code != http.StatusBadRequest {
		t.Errorf("no tenant: %d", code)
	}
}
