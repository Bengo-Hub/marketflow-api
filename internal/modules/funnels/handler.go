package funnels

import (
	"fmt"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for funnels.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new funnel handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("funnels.handler"),
	}
}

type createFunnelRequest struct {
	Name            string           `json:"name"`
	Slug            string           `json:"slug"`
	FunnelType      *string          `json:"funnel_type"`
	Steps           []map[string]any `json:"steps"`
	LandingPageHTML *string          `json:"landing_page_html"`
	LandingPageCSS  map[string]any   `json:"landing_page_css"`
	Metadata        map[string]any   `json:"metadata"`
	legacyFields
}

type updateFunnelRequest struct {
	Name            *string          `json:"name"`
	Slug            *string          `json:"slug"`
	FunnelType      *string          `json:"funnel_type"`
	Status          *string          `json:"status"`
	Steps           []map[string]any `json:"steps"`
	LandingPageHTML *string          `json:"landing_page_html"`
	LandingPageCSS  map[string]any   `json:"landing_page_css"`
	Metadata        map[string]any   `json:"metadata"`
	legacyFields
}

// Create handles POST /api/v1/funnels
// @Summary Create a new funnel
// @Description Creates a multi-step landing page funnel
// @Tags Funnels
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createFunnelRequest true "Funnel data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 500 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /funnels [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req createFunnelRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.Name == "" || req.Slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "name and slug are required", nil)
		return
	}

	req.Steps, req.Metadata = req.legacyFields.fold(req.Steps, req.Metadata)

	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:        tenantID,
		Name:            req.Name,
		Slug:            req.Slug,
		FunnelType:      req.FunnelType,
		Steps:           req.Steps,
		LandingPageHTML: req.LandingPageHTML,
		LandingPageCSS:  req.LandingPageCSS,
		Metadata:        req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to create funnel", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create funnel", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/funnels
// @Summary List funnels
// @Description Returns paginated funnels with optional filters
// @Tags Funnels
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param status query string false "Filter by status: draft, published, archived"
// @Param funnel_type query string false "Filter by type: product_inquiry, recruitment, c9_challenge, generic"
// @Param limit query int false "Page size (default 20, max 100)"
// @Param offset query int false "Offset for pagination"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /funnels [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	status := r.URL.Query().Get("status")
	funnelType := r.URL.Query().Get("funnel_type")

	p := pagination.Parse(r)
	input := ListInput{
		TenantID: tenantID,
		Limit:    p.Limit,
		Offset:   p.Offset,
	}
	if status != "" {
		input.Status = &status
	}
	if funnelType != "" {
		input.FunnelType = &funnelType
	}

	results, total, err := h.service.List(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to list funnels", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list funnels", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
}

// Get handles GET /api/v1/funnels/{id}
// @Summary Get a funnel by ID
// @Tags Funnels
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Funnel UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /funnels/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	funnelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid funnel ID", nil)
		return
	}

	result, err := h.service.Get(r.Context(), tenantID, funnelID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "funnel not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/funnels/{id}
// @Summary Update a funnel
// @Tags Funnels
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Funnel UUID"
// @Param request body updateFunnelRequest true "Fields to update"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /funnels/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	funnelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid funnel ID", nil)
		return
	}

	var req updateFunnelRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	req.Steps, req.Metadata = req.legacyFields.fold(req.Steps, req.Metadata)

	result, err := h.service.Update(r.Context(), tenantID, funnelID, UpdateInput{
		Name:            req.Name,
		Slug:            req.Slug,
		FunnelType:      req.FunnelType,
		Status:          req.Status,
		Steps:           req.Steps,
		LandingPageHTML: req.LandingPageHTML,
		LandingPageCSS:  req.LandingPageCSS,
		Metadata:        req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to update funnel", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update funnel", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/funnels/{id}
// @Summary Delete a funnel (marks as archived)
// @Tags Funnels
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Funnel UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /funnels/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	funnelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid funnel ID", nil)
		return
	}

	if err := h.service.Delete(r.Context(), tenantID, funnelID); err != nil {
		h.logger.Error("failed to delete funnel", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete funnel", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// Publish handles POST /api/v1/funnels/{id}/publish
// @Summary Publish a funnel
// @Description Sets funnel status to published and records published_at timestamp
// @Tags Funnels
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Funnel UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /funnels/{id}/publish [post]
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	funnelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid funnel ID", nil)
		return
	}

	result, err := h.service.Publish(r.Context(), tenantID, funnelID)
	if err != nil {
		h.logger.Error("failed to publish funnel", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "publish_failed", "failed to publish funnel", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// PublicGetBySlug handles GET /f/{slug} — returns a published funnel by slug.
func (h *Handler) PublicGetBySlug(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return
	}

	result, err := h.service.GetBySlug(r.Context(), slug)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "funnel not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// extractTenantID extracts tenant ID from X-Tenant-ID header.
func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

