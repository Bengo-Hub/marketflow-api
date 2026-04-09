package leads

import (
	"fmt"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for leads.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new lead handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("leads.handler"),
	}
}

type createLeadRequest struct {
	Email                *string        `json:"email"`
	Phone                *string        `json:"phone"`
	FirstName            *string        `json:"first_name"`
	LastName             *string        `json:"last_name"`
	Source               string         `json:"source"`
	CampaignID           *uuid.UUID     `json:"campaign_id"`
	FunnelID             *uuid.UUID     `json:"funnel_id"`
	UTMSource            *string        `json:"utm_source"`
	UTMMedium            *string        `json:"utm_medium"`
	UTMCampaign          *string        `json:"utm_campaign"`
	LeadType             string         `json:"lead_type"`
	Interest             *string        `json:"interest"`
	QualificationAnswers map[string]any `json:"qualification_answers"`
	Metadata             map[string]any `json:"metadata"`
}

type updateLeadRequest struct {
	Email                *string        `json:"email"`
	Phone                *string        `json:"phone"`
	FirstName            *string        `json:"first_name"`
	LastName             *string        `json:"last_name"`
	Status               *string        `json:"status"`
	Score                *int           `json:"score"`
	Interest             *string        `json:"interest"`
	QualificationAnswers map[string]any `json:"qualification_answers"`
	Metadata             map[string]any `json:"metadata"`
}

// Create handles POST /api/v1/leads
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req createLeadRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.Source == "" {
		req.Source = "manual"
	}
	if req.LeadType == "" {
		req.LeadType = "customer"
	}

	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:             tenantID,
		Email:                req.Email,
		Phone:                req.Phone,
		FirstName:            req.FirstName,
		LastName:             req.LastName,
		Source:               req.Source,
		CampaignID:           req.CampaignID,
		FunnelID:             req.FunnelID,
		UTMSource:            req.UTMSource,
		UTMMedium:            req.UTMMedium,
		UTMCampaign:          req.UTMCampaign,
		LeadType:             req.LeadType,
		Interest:             req.Interest,
		QualificationAnswers: req.QualificationAnswers,
		Metadata:             req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to create lead", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create lead", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/leads
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	status := r.URL.Query().Get("status")
	source := r.URL.Query().Get("source")
	leadType := r.URL.Query().Get("lead_type")

	input := ListInput{
		TenantID: tenantID,
		Limit:    parseIntQuery(r, "limit", 20),
		Offset:   parseIntQuery(r, "offset", 0),
	}
	if status != "" {
		input.Status = &status
	}
	if source != "" {
		input.Source = &source
	}
	if leadType != "" {
		input.LeadType = &leadType
	}

	results, total, err := h.service.List(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to list leads", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list leads", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"data":  results,
		"total": total,
	})
}

// Get handles GET /api/v1/leads/{id}
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	leadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid lead ID", nil)
		return
	}

	result, err := h.service.Get(r.Context(), tenantID, leadID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "lead not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/leads/{id}
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	leadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid lead ID", nil)
		return
	}

	var req updateLeadRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	result, err := h.service.Update(r.Context(), tenantID, leadID, UpdateInput{
		Email:                req.Email,
		Phone:                req.Phone,
		FirstName:            req.FirstName,
		LastName:             req.LastName,
		Status:               req.Status,
		Score:                req.Score,
		Interest:             req.Interest,
		QualificationAnswers: req.QualificationAnswers,
		Metadata:             req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to update lead", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update lead", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/leads/{id}
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	leadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid lead ID", nil)
		return
	}

	if err := h.service.Delete(r.Context(), tenantID, leadID); err != nil {
		h.logger.Error("failed to delete lead", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete lead", nil)
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
