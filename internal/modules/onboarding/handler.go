package onboarding

import (
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Handler serves the onboarding endpoints.
type Handler struct {
	svc    *Service
	logger *zap.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, logger: logger.Named("onboarding")}
}

func tenantID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	return id, err == nil
}

// Checklist handles GET /api/v1/tenant/onboarding.
// @Summary Setup checklist
// @Description Progress through setting up MarketFlow, computed from existing data
// @Tags Onboarding
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} Checklist
// @Security bearerAuth
// @Router /tenant/onboarding [get]
func (h *Handler) Checklist(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	c, err := h.svc.Checklist(r.Context(), tid)
	if err != nil {
		h.logger.Error("checklist", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "checklist_failed", "could not load the setup checklist", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, c)
}

// Dismiss handles POST /api/v1/tenant/onboarding/dismiss {"dismissed": true|false}.
// @Summary Hide or show the setup prompt
// @Tags Onboarding
// @Accept json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 204
// @Security bearerAuth
// @Router /tenant/onboarding/dismiss [post]
func (h *Handler) Dismiss(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	var body struct {
		Dismissed bool `json:"dismissed"`
	}
	if err := httpapi.DecodeJSON(r, &body); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	if err := h.svc.SetDismissed(r.Context(), tid, body.Dismissed); err != nil {
		h.logger.Error("dismiss", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Draft handles GET /api/v1/tenant/marketing-context/draft.
// @Summary Suggested brand context
// @Description Built from the profile, the account details, best sellers (or the website for the platform tenant) and testimonials. Nothing is saved; PUT /tenant/marketing-context saves.
// @Tags Onboarding
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} Draft
// @Security bearerAuth
// @Router /tenant/marketing-context/draft [get]
func (h *Handler) Draft(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	d, err := h.svc.Draft(r.Context(), tid)
	if err != nil {
		h.logger.Error("draft", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "draft_failed", "could not build a draft", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, d)
}
