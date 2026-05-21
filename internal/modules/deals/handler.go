package deals

import (
	"fmt"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for deals.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new deal handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("deals.handler"),
	}
}

type createDealRequest struct {
	Title             string         `json:"title"`
	ContactID         *uuid.UUID     `json:"contact_id"`
	LeadID            *uuid.UUID     `json:"lead_id"`
	AccountID         *uuid.UUID     `json:"account_id"`
	PipelineID        uuid.UUID      `json:"pipeline_id"`
	StageID           uuid.UUID      `json:"stage_id"`
	Value             *float64       `json:"value"`
	Currency          string         `json:"currency"`
	Probability       *float64       `json:"probability"`
	ExpectedCloseDate *string        `json:"expected_close_date"`
	OwnerUserID       *uuid.UUID     `json:"owner_user_id"`
	Metadata          map[string]any `json:"metadata"`
}

type updateDealRequest struct {
	Title             *string        `json:"title"`
	ContactID         *uuid.UUID     `json:"contact_id"`
	AccountID         *uuid.UUID     `json:"account_id"`
	StageID           *uuid.UUID     `json:"stage_id"`
	Value             *float64       `json:"value"`
	Currency          *string        `json:"currency"`
	Probability       *float64       `json:"probability"`
	ExpectedCloseDate *string        `json:"expected_close_date"`
	OwnerUserID       *uuid.UUID     `json:"owner_user_id"`
	Metadata          map[string]any `json:"metadata"`
}

type moveStageRequest struct {
	StageID uuid.UUID `json:"stage_id"`
}

type markLostRequest struct {
	Reason *string `json:"reason"`
}

// Create handles POST /api/v1/deals
// @Summary Create a deal
// @Tags Deals
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createDealRequest true "Deal data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /deals [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req createDealRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.Title == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_title", "title is required", nil)
		return
	}

	in := CreateInput{
		TenantID:    tenantID,
		Title:       req.Title,
		ContactID:   req.ContactID,
		LeadID:      req.LeadID,
		AccountID:   req.AccountID,
		PipelineID:  req.PipelineID,
		StageID:     req.StageID,
		Value:       req.Value,
		Currency:    req.Currency,
		Probability: req.Probability,
		OwnerUserID: req.OwnerUserID,
		Metadata:    req.Metadata,
	}
	if req.ExpectedCloseDate != nil {
		t, err := parseDate(*req.ExpectedCloseDate)
		if err == nil {
			in.ExpectedCloseDate = &t
		}
	}

	result, err := h.service.Create(r.Context(), in)
	if err != nil {
		h.logger.Error("failed to create deal", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create deal", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/deals
// @Summary List deals
// @Tags Deals
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param status query string false "open | won | lost"
// @Param pipeline_id query string false "Filter by pipeline UUID"
// @Param stage_id query string false "Filter by stage UUID"
// @Param contact_id query string false "Filter by contact UUID"
// @Param limit query int false "Page size (default 20)"
// @Param offset query int false "Offset"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /deals [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	in := ListInput{
		TenantID: tenantID,
		Limit:    parseIntQuery(r, "limit", 20),
		Offset:   parseIntQuery(r, "offset", 0),
	}
	if s := r.URL.Query().Get("status"); s != "" {
		in.Status = &s
	}
	if s := r.URL.Query().Get("pipeline_id"); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			in.PipelineID = &id
		}
	}
	if s := r.URL.Query().Get("stage_id"); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			in.StageID = &id
		}
	}
	if s := r.URL.Query().Get("contact_id"); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			in.ContactID = &id
		}
	}
	if s := r.URL.Query().Get("account_id"); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			in.AccountID = &id
		}
	}
	if s := r.URL.Query().Get("owner_id"); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			in.OwnerID = &id
		}
	}

	results, total, err := h.service.List(r.Context(), in)
	if err != nil {
		h.logger.Error("failed to list deals", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list deals", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{"data": results, "total": total})
}

// Get handles GET /api/v1/deals/{id}
// @Summary Get a deal by ID
// @Tags Deals
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Deal UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /deals/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid deal ID", nil)
		return
	}
	result, err := h.service.Get(r.Context(), tenantID, id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "deal not found", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/deals/{id}
// @Summary Update a deal
// @Tags Deals
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Deal UUID"
// @Param request body updateDealRequest true "Fields to update"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /deals/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid deal ID", nil)
		return
	}

	var req updateDealRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	in := UpdateInput{
		Title:       req.Title,
		ContactID:   req.ContactID,
		AccountID:   req.AccountID,
		StageID:     req.StageID,
		Value:       req.Value,
		Currency:    req.Currency,
		Probability: req.Probability,
		OwnerUserID: req.OwnerUserID,
		Metadata:    req.Metadata,
	}
	if req.ExpectedCloseDate != nil {
		t, err := parseDate(*req.ExpectedCloseDate)
		if err == nil {
			in.ExpectedCloseDate = &t
		}
	}

	result, err := h.service.Update(r.Context(), tenantID, id, in)
	if err != nil {
		h.logger.Error("failed to update deal", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update deal", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Move handles POST /api/v1/deals/{id}/move
// @Summary Move a deal to a new pipeline stage
// @Tags Deals
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Deal UUID"
// @Param request body moveStageRequest true "Target stage"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /deals/{id}/move [post]
func (h *Handler) Move(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid deal ID", nil)
		return
	}

	var req moveStageRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	result, err := h.service.Move(r.Context(), tenantID, id, req.StageID)
	if err != nil {
		h.logger.Error("failed to move deal", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "move_failed", "failed to move deal", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// MarkWon handles POST /api/v1/deals/{id}/won
// @Summary Mark a deal as won
// @Tags Deals
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Deal UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /deals/{id}/won [post]
func (h *Handler) MarkWon(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid deal ID", nil)
		return
	}

	result, err := h.service.MarkWon(r.Context(), tenantID, id)
	if err != nil {
		h.logger.Error("failed to mark deal won", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "won_failed", "failed to mark deal as won", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// MarkLost handles POST /api/v1/deals/{id}/lost
// @Summary Mark a deal as lost
// @Tags Deals
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Deal UUID"
// @Param request body markLostRequest false "Optional reason"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /deals/{id}/lost [post]
func (h *Handler) MarkLost(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid deal ID", nil)
		return
	}

	var req markLostRequest
	_ = httpapi.DecodeJSON(r, &req)

	result, err := h.service.MarkLost(r.Context(), tenantID, id, req.Reason)
	if err != nil {
		h.logger.Error("failed to mark deal lost", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "lost_failed", "failed to mark deal as lost", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/deals/{id}
// @Summary Delete a deal
// @Tags Deals
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Deal UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /deals/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid deal ID", nil)
		return
	}

	if err := h.service.Delete(r.Context(), tenantID, id); err != nil {
		h.logger.Error("failed to delete deal", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete deal", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// extractTenantID extracts tenant ID from X-Tenant-ID header.
func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

// parseIntQuery parses an integer query parameter with a default value.
func parseIntQuery(r *http.Request, key string, defaultVal int) int {
	val := r.URL.Query().Get(key)
	if val == "" {
		return defaultVal
	}
	n := 0
	for _, c := range val {
		if c < '0' || c > '9' {
			return defaultVal
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// parseDate parses a date string in RFC3339 or YYYY-MM-DD format.
func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}
