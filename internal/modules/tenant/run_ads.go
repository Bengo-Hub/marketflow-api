package tenant

import (
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/platform/runads"
)

// GetRunAds handles GET /api/v1/tenant/profile/run-ads: the tenant's Run ads defaults (platforms,
// budget tier or own daily amounts, days, total cap) beside the platform defaults they override,
// in the tenant's currency. Saved through the profile PATCH as metadata.run_ads by the approving
// roles.
// @Summary Run ads defaults
// @Tags Tenant
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/profile/run-ads [get]
func (h *Handler) GetRunAds(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantHeader(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	t, err := h.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "tenant not found", nil)
		return
	}
	effective, platform := runads.Load(ctx, h.db, t)
	override, _ := t.Metadata[runads.TenantKey].(map[string]any)
	if override == nil {
		override = map[string]any{}
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"platform":  platform.ToView(),
		"effective": effective.ToView(),
		"tenant":    override,
		"currency":  CurrencyOf(ctx, h.db, tenantID, PlatformCurrency),
		"can_edit":  h.CanApprove == nil || h.CanApprove(ctx, tenantID),
	})
}
