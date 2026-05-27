package leads

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
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
// @Summary Create a new lead
// @Description Creates a lead from an ad, funnel, chatbot, or manual entry
// @Tags Leads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createLeadRequest true "Lead data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 500 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /leads [post]
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
// @Summary List leads
// @Description Returns paginated leads with optional filters
// @Tags Leads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param status query string false "Filter by status: new, contacted, qualified, converted, lost"
// @Param source query string false "Filter by source: meta_ad, organic, referral, chatbot, whatsapp, manual"
// @Param lead_type query string false "Filter by type: customer, recruit"
// @Param limit query int false "Page size (default 20, max 100)"
// @Param offset query int false "Offset for pagination"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /leads [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	status := r.URL.Query().Get("status")
	source := r.URL.Query().Get("source")
	leadType := r.URL.Query().Get("lead_type")

	p := pagination.Parse(r)
	input := ListInput{
		TenantID: tenantID,
		Limit:    p.Limit,
		Offset:   p.Offset,
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
	if fromStr := r.URL.Query().Get("from"); fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			input.From = &t
		} else if t, err := time.Parse("2006-01-02", fromStr); err == nil {
			input.From = &t
		}
	}
	if toStr := r.URL.Query().Get("to"); toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			input.To = &t
		} else if t, err := time.Parse("2006-01-02", toStr); err == nil {
			eod := t.Add(24*time.Hour - time.Nanosecond)
			input.To = &eod
		}
	}

	results, total, err := h.service.List(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to list leads", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list leads", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
}

// Get handles GET /api/v1/leads/{id}
// @Summary Get a lead by ID
// @Tags Leads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Lead UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /leads/{id} [get]
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
// @Summary Update a lead
// @Tags Leads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Lead UUID"
// @Param request body updateLeadRequest true "Fields to update"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /leads/{id} [put]
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
// @Summary Delete a lead (marks as lost)
// @Tags Leads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Lead UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /leads/{id} [delete]
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

// Export handles GET /api/v1/leads/export
// @Summary Export leads as CSV
// @Description Returns leads matching the given filters as a downloadable CSV file
// @Tags Leads
// @Produce text/csv
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param source query string false "Filter by source"
// @Param lead_type query string false "Filter by type"
// @Param from query string false "Start date (RFC3339 or YYYY-MM-DD)"
// @Param to query string false "End date (RFC3339 or YYYY-MM-DD)"
// @Success 200 {file} file "CSV file"
// @Security bearerAuth
// @Router /leads/export [get]
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	input := ListInput{
		TenantID: tenantID,
	}
	if source := r.URL.Query().Get("source"); source != "" {
		input.Source = &source
	}
	if leadType := r.URL.Query().Get("lead_type"); leadType != "" {
		input.LeadType = &leadType
	}
	if fromStr := r.URL.Query().Get("from"); fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			input.From = &t
		} else if t, err := time.Parse("2006-01-02", fromStr); err == nil {
			input.From = &t
		}
	}
	if toStr := r.URL.Query().Get("to"); toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			input.To = &t
		} else if t, err := time.Parse("2006-01-02", toStr); err == nil {
			eod := t.Add(24*time.Hour - time.Nanosecond)
			input.To = &eod
		}
	}

	results, err := h.service.ListAll(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to export leads", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "export_failed", "failed to export leads", nil)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=leads.csv")
	w.WriteHeader(http.StatusOK)

	writer := csv.NewWriter(w)
	// Write header row
	_ = writer.Write([]string{
		"email", "phone", "first_name", "last_name", "source", "lead_type",
		"utm_source", "utm_medium", "utm_campaign", "interest", "created_at",
	})

	// Stream rows directly
	for _, l := range results {
		row := []string{
			derefStr(l.Email),
			derefStr(l.Phone),
			derefStr(l.FirstName),
			derefStr(l.LastName),
			l.Source,
			l.LeadType,
			derefStr(l.UtmSource),
			derefStr(l.UtmMedium),
			derefStr(l.UtmCampaign),
			derefStr(l.Interest),
			l.CreatedAt.Format(time.RFC3339),
		}
		_ = writer.Write(row)
	}
	writer.Flush()
}

// ingestFromServiceRequest is the body accepted by POST /api/v1/internal/leads.
// Called by Codevertex platform services (codevertex-website, ordering-backend, etc.)
// to push leads into MarketFlow via the shared INTERNAL_SERVICE_KEY.
type ingestFromServiceRequest struct {
	ServiceOrigin string         `json:"service_origin"` // e.g. "codevertex-website", "ordering-backend"
	Email         *string        `json:"email"`
	Phone         *string        `json:"phone"`
	FirstName     *string        `json:"first_name"`
	LastName      *string        `json:"last_name"`
	LeadType      string         `json:"lead_type"`   // customer | recruit
	Interest      *string        `json:"interest"`
	UTMSource     *string        `json:"utm_source"`
	UTMMedium     *string        `json:"utm_medium"`
	UTMCampaign   *string        `json:"utm_campaign"`
	Metadata      map[string]any `json:"metadata"`
}

// IngestFromService handles POST /api/v1/internal/leads.
// Authenticated by X-API-Key (INTERNAL_SERVICE_KEY); no JWT required.
// X-Tenant-ID must be a valid UUID identifying the target tenant.
func (h *Handler) IngestFromService(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req ingestFromServiceRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.Email == nil && req.Phone == nil {
		httpapi.Error(w, http.StatusBadRequest, "missing_contact", "at least one of email or phone is required", nil)
		return
	}
	if req.ServiceOrigin == "" {
		req.ServiceOrigin = "unknown"
	}
	if req.LeadType == "" {
		req.LeadType = "customer"
	}

	// Merge service_origin into metadata so it's queryable from the CRM.
	meta := req.Metadata
	if meta == nil {
		meta = make(map[string]any)
	}
	meta["service_origin"] = req.ServiceOrigin

	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:    tenantID,
		Email:       req.Email,
		Phone:       req.Phone,
		FirstName:   req.FirstName,
		LastName:    req.LastName,
		Source:      "platform_service",
		UTMSource:   req.UTMSource,
		UTMMedium:   req.UTMMedium,
		UTMCampaign: req.UTMCampaign,
		LeadType:    req.LeadType,
		Interest:    req.Interest,
		Metadata:    meta,
	})
	if err != nil {
		h.logger.Error("failed to ingest service lead", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "ingest_failed", "failed to create lead", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, result)
}

// derefStr safely dereferences a string pointer.
func derefStr(s *string) string {
	if s != nil {
		return *s
	}
	return ""
}

// extractTenantID extracts tenant ID from X-Tenant-ID header.
func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

