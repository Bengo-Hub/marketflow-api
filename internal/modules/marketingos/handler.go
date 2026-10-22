package marketingos

import (
	"encoding/json"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves the Marketing OS routes of marketflow-api.
type Handler struct {
	svc    *Service
	logger *zap.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, logger: logger.Named("marketingos.handler")}
}

// tenantOf reads the tenant the protected group's tenant check has already
// verified against the token (httpware TenantV2 in the router).
func tenantOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}

// GetBrandContext handles GET /api/v1/tenant/marketing-context.
// @Summary Get the tenant's brand context (Marketing OS)
// @Tags MarketingOS
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} BrandContext
// @Security bearerAuth
// @Router /tenant/marketing-context [get]
func (h *Handler) GetBrandContext(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	bc, err := h.svc.GetBrandContext(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, bc)
}

// PutBrandContext handles PUT /api/v1/tenant/marketing-context.
// @Summary Save the tenant's brand context (Marketing OS)
// @Tags MarketingOS
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body BrandContext true "Brand context"
// @Success 200 {object} BrandContext
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/marketing-context [put]
func (h *Handler) PutBrandContext(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	var bc BrandContext
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&bc); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	saved, err := h.svc.SaveBrandContext(r.Context(), tid, bc)
	if err != nil {
		if ent.IsNotFound(err) {
			h.fail(w, err)
			return
		}
		httpapi.Error(w, http.StatusBadRequest, "invalid_context", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, saved)
}

// ListTeam handles GET /api/v1/agents/team: the tenant's member overrides. The
// full roster with defaults is served by marketflow-ai at /api/v1/os/team.
// @Summary List the tenant's Marketing OS team overrides
// @Tags MarketingOS
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/team [get]
func (h *Handler) ListTeam(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	members, err := h.svc.TeamOverrides(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"members": members})
}

// PutTeamMember handles PUT /api/v1/agents/team/{key}: rename a member, switch it
// off, change its model class or add instructions.
// @Summary Update a Marketing OS team member
// @Tags MarketingOS
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param key path string true "Member key, for example ads_specialist"
// @Param request body MemberOverride true "Override"
// @Success 200 {object} MemberOverride
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /agents/team/{key} [put]
func (h *Handler) PutTeamMember(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	var o MemberOverride
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&o); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	o.Key = chi.URLParam(r, "key")
	saved, err := h.svc.UpsertTeamMember(r.Context(), tid, o)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_member", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, saved)
}

// InternalContext handles GET /api/v1/internal/os/context?tenant_id= (service
// key only): what marketflow-ai reads for each Marketing OS request.
func (h *Handler) InternalContext(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "tenant_id must be a UUID", nil)
		return
	}
	out, err := h.svc.Context(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, out)
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if ent.IsNotFound(err) {
		httpapi.Error(w, http.StatusNotFound, "not_found", "tenant not found", nil)
		return
	}
	h.logger.Error("marketing os request failed", zap.Error(err))
	httpapi.Error(w, http.StatusInternalServerError, "internal", "request failed", nil)
}
