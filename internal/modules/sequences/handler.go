package sequences

import (
	"fmt"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for nurture sequences.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new nurture sequence handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("sequences.handler"),
	}
}

type createSequenceRequest struct {
	Name           string           `json:"name"`
	TriggerEvent   string           `json:"trigger_event"`
	LeadTypeFilter *string          `json:"lead_type_filter"`
	Active         *bool            `json:"active"`
	Steps          []map[string]any `json:"steps"`
	Metadata       map[string]any   `json:"metadata"`
}

type updateSequenceRequest struct {
	Name           *string          `json:"name"`
	TriggerEvent   *string          `json:"trigger_event"`
	LeadTypeFilter *string          `json:"lead_type_filter"`
	Active         *bool            `json:"active"`
	Steps          []map[string]any `json:"steps"`
	Metadata       map[string]any   `json:"metadata"`
}

// Create handles POST /api/v1/sequences
// @Summary Create a new nurture sequence
// @Description Creates an automated email/notification sequence triggered by lead lifecycle events
// @Tags NurtureSequences
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createSequenceRequest true "Sequence data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 500 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /sequences [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req createSequenceRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.Name == "" || req.TriggerEvent == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "name and trigger_event are required", nil)
		return
	}

	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:       tenantID,
		Name:           req.Name,
		TriggerEvent:   req.TriggerEvent,
		LeadTypeFilter: req.LeadTypeFilter,
		Active:         req.Active,
		Steps:          req.Steps,
		Metadata:       req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to create sequence", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create sequence", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/sequences
// @Summary List nurture sequences
// @Description Returns paginated nurture sequences with optional filters
// @Tags NurtureSequences
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param trigger_event query string false "Filter by trigger event: lead.created, lead.qualified, contact.dormant"
// @Param active query string false "Filter by active status: true, false"
// @Param limit query int false "Page size (default 20, max 100)"
// @Param offset query int false "Offset for pagination"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /sequences [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	triggerEvent := r.URL.Query().Get("trigger_event")
	activeStr := r.URL.Query().Get("active")

	p := pagination.Parse(r)
	input := ListInput{
		TenantID: tenantID,
		Limit:    p.Limit,
		Offset:   p.Offset,
	}
	if triggerEvent != "" {
		input.TriggerEvent = &triggerEvent
	}
	if activeStr != "" {
		active := activeStr == "true"
		input.Active = &active
	}

	results, total, err := h.service.List(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to list sequences", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list sequences", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
}

// Get handles GET /api/v1/sequences/{id}
// @Summary Get a nurture sequence by ID
// @Tags NurtureSequences
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Sequence UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /sequences/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	sequenceID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid sequence ID", nil)
		return
	}

	result, err := h.service.Get(r.Context(), tenantID, sequenceID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "sequence not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/sequences/{id}
// @Summary Update a nurture sequence
// @Tags NurtureSequences
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Sequence UUID"
// @Param request body updateSequenceRequest true "Fields to update"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /sequences/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	sequenceID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid sequence ID", nil)
		return
	}

	var req updateSequenceRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	result, err := h.service.Update(r.Context(), tenantID, sequenceID, UpdateInput{
		Name:           req.Name,
		TriggerEvent:   req.TriggerEvent,
		LeadTypeFilter: req.LeadTypeFilter,
		Active:         req.Active,
		Steps:          req.Steps,
		Metadata:       req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to update sequence", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update sequence", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/sequences/{id}
// @Summary Delete a nurture sequence (deactivates it)
// @Tags NurtureSequences
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Sequence UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /sequences/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	sequenceID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid sequence ID", nil)
		return
	}

	if err := h.service.Delete(r.Context(), tenantID, sequenceID); err != nil {
		h.logger.Error("failed to delete sequence", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete sequence", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// Analytics handles GET /api/v1/sequences/{id}/analytics
// @Summary Sequence enrollment analytics
// @Description Returns enrollment counts grouped by status and a completion rate for the given sequence
// @Tags NurtureSequences
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Sequence UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /sequences/{id}/analytics [get]
func (h *Handler) Analytics(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	sequenceID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid sequence ID", nil)
		return
	}

	analytics, err := h.service.Analytics(r.Context(), tenantID, sequenceID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "sequence not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, analytics)
}

// extractTenantID extracts tenant ID from X-Tenant-ID header.
func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

