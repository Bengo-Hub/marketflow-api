package rbac

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// accessResponse is the caller's MarketFlow access as the UI needs it to shape its menu and
// buttons. The API gates stay the authority; this only spares the UI calls it would be refused.
type accessResponse struct {
	// Role is the effective MarketFlow role (the highest held), or "" when the caller has none here.
	Role  string   `json:"role"`
	Roles []string `json:"roles"`
	// Restricted: the role works from an explicit allow-list (the app reviewer), not the ladder.
	Restricted    bool `json:"restricted"`
	PlatformOwner bool `json:"platform_owner"`
}

// MyAccess handles GET /api/v1/me/access: the signed-in caller's MarketFlow role in the tenant
// they act for. Open to every role, the app reviewer included (it cannot read the team list).
// @Summary The caller's MarketFlow role
// @Tags Tenant
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /me/access [get]
func (s *Service) MyAccess(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	a := s.callerAccess(r.Context(), tid)
	roles := a.roles
	if roles == nil {
		roles = []string{}
	}
	httpapi.JSON(w, http.StatusOK, accessResponse{
		Role:          effectiveRole(roles),
		Roles:         roles,
		Restricted:    !a.bypass && restrictedRole(roles) != "",
		PlatformOwner: a.platformOwner,
	})
}
