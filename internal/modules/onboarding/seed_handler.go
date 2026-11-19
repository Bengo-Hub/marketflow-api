package onboarding

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// dryRun reads ?dry_run=; anything but an explicit false is a dry run, so a typo never writes.
func dryRun(r *http.Request) bool {
	v := r.URL.Query().Get("dry_run")
	return v != "false" && v != "0"
}

// SeedTenant handles POST /api/v1/admin/tenants/{tenantID}/seed-defaults?dry_run=true.
// @Summary Seed one tenant's defaults (fill gaps only)
// @Description Fills only empty brand context, profile and FAQ fields from the tenant's own sources (account record, website, POS and inventory, testimonials) and applies missing platform defaults. dry_run defaults to true; pass dry_run=false to write. Platform owner only.
// @Tags Admin - Tenants
// @Produce json
// @Param tenantID path string true "Tenant UUID"
// @Param dry_run query bool false "Report only (default true)"
// @Success 200 {object} SeedReport
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /admin/tenants/{tenantID}/seed-defaults [post]
func (h *Handler) SeedTenant(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "tenant id must be a UUID", nil)
		return
	}
	rep, err := h.svc.SeedDefaults(r.Context(), id, dryRun(r))
	if err != nil {
		if ent.IsNotFound(err) {
			httpapi.Error(w, http.StatusNotFound, "not_found", "tenant not found", nil)
			return
		}
		h.logger.Error("seed defaults", zap.String("tenant_id", id.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "seed_failed", "could not seed defaults", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, rep)
}

// SeedAll handles POST /api/v1/admin/tenants/seed-defaults?dry_run=true&offset=0&limit=5: one page
// of active tenants, ordered by slug.
// @Summary Seed defaults for active tenants (fill gaps only)
// @Description One page of active tenants (limit at most 20). dry_run defaults to true. Platform owner only.
// @Tags Admin - Tenants
// @Produce json
// @Param dry_run query bool false "Report only (default true)"
// @Param offset query int false "Offset"
// @Param limit query int false "Page size (default 5, at most 20)"
// @Success 200 {object} SeedPage
// @Security bearerAuth
// @Router /admin/tenants/seed-defaults [post]
func (h *Handler) SeedAll(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := h.svc.SeedActiveTenants(r.Context(), offset, limit, dryRun(r))
	if err != nil {
		h.logger.Error("seed defaults (all)", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "seed_failed", "could not seed defaults", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, page)
}
