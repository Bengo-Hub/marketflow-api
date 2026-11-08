package marketingos

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

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
	// CanManage reports whether the caller may change the team (the approving roles). Nil
	// allows everyone, for tests.
	CanManage func(ctx context.Context, tenantID uuid.UUID) bool
}

// manager refuses a caller who may not change the team: its members speak and spend for the
// business, so only the owner or an admin may switch them or rewrite their instructions.
func (h *Handler) manager(w http.ResponseWriter, r *http.Request, tid uuid.UUID) bool {
	if h.CanManage != nil && !h.CanManage(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can change the marketing team", nil)
		return false
	}
	return true
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
	// Onboarding: the first brand context sets up the prebuilt team, once and for free. It runs
	// after the reply so the owner's save is never slowed or failed by it.
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		h.svc.SetupTeamIfNew(ctx, tid)
	}()
	httpapi.JSON(w, http.StatusOK, saved)
}

// SetupTeam handles POST /api/v1/agents/team/setup: fit the prebuilt team to the business and
// return the result for the org chart.
// @Summary Set up or review the Marketing OS team
// @Tags MarketingOS
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} TeamSetup
// @Failure 403 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /agents/team/setup [post]
func (h *Handler) SetupTeam(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok || !h.manager(w, r, tid) {
		return
	}
	h.setup(w, r, tid)
}

// InternalSetupTeam handles POST /api/v1/internal/os/team/setup?tenant_id= (service key only):
// marketflow-ai runs it when the owner asks Vera, in the portal chat or on WhatsApp, to set up or
// review the team. Who may ask is checked there (WhatsApp sender identity, portal roles).
func (h *Handler) InternalSetupTeam(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "tenant_id must be a UUID", nil)
		return
	}
	h.setup(w, r, tid)
}

func (h *Handler) setup(w http.ResponseWriter, r *http.Request, tid uuid.UUID) {
	out, err := h.svc.SetupTeam(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, out)
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
	if !ok || !h.manager(w, r, tid) {
		return
	}
	var o MemberOverride
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&o); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	o.Key = chi.URLParam(r, "key")
	o.SetBy = "" // a change through this route is always the owner's own
	saved, err := h.svc.UpsertTeamMember(r.Context(), tid, o)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_member", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, saved)
}

// GetSettings handles GET /api/v1/tenant/os-settings.
// @Summary Get the tenant's Marketing OS switches
// @Tags MarketingOS
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} Settings
// @Security bearerAuth
// @Router /tenant/os-settings [get]
func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	st, err := h.svc.GetSettings(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, st)
}

// PutSettings handles PUT /api/v1/tenant/os-settings.
// @Summary Save the tenant's Marketing OS switches (daily brief on or off)
// @Tags MarketingOS
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body Settings true "Switches"
// @Success 200 {object} Settings
// @Security bearerAuth
// @Router /tenant/os-settings [put]
func (h *Handler) PutSettings(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	// Start from the saved switches, so a body that names only some of them (the brief page
	// sends daily_brief alone) never resets the others.
	in, err := h.svc.GetSettings(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	// 32 KiB holds 20 reference ads with notes, 20 complaints and the other switches.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	saved, err := h.svc.SaveSettings(r.Context(), tid, in)
	if err != nil {
		h.fail(w, err)
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
	if errors.Is(err, ErrInvalidSettings) {
		httpapi.Error(w, http.StatusBadRequest, "invalid_settings", strings.TrimPrefix(err.Error(), ErrInvalidSettings.Error()+": "), nil)
		return
	}
	h.logger.Error("marketing os request failed", zap.Error(err))
	httpapi.Error(w, http.StatusInternalServerError, "internal", "request failed", nil)
}
