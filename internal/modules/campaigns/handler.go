package campaigns

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for campaigns.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new campaign handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("campaigns.handler"),
	}
}

type createCampaignRequest struct {
	Name           string         `json:"name"`
	CampaignType   *string        `json:"campaign_type"`
	Platform       *string        `json:"platform"`
	Channel        *string        `json:"channel"`
	MetaCampaignID *string        `json:"meta_campaign_id"`
	MetaAdsetID    *string        `json:"meta_adset_id"`
	MetaAdID       *string        `json:"meta_ad_id"`
	BudgetDaily    *float64       `json:"budget_daily"`
	BudgetTotal    *float64       `json:"budget_total"`
	Currency       *string        `json:"currency"`
	TargetAudience map[string]any `json:"target_audience"`
	FunnelID       *uuid.UUID     `json:"funnel_id"`
	StartDate      *time.Time     `json:"start_date"`
	EndDate        *time.Time     `json:"end_date"`
	Metrics        map[string]any `json:"metrics"`
	Metadata       map[string]any `json:"metadata"`
}

type updateCampaignRequest struct {
	Name           *string        `json:"name"`
	CampaignType   *string        `json:"campaign_type"`
	Platform       *string        `json:"platform"`
	Channel        *string        `json:"channel"`
	Status         *string        `json:"status"`
	MetaCampaignID *string        `json:"meta_campaign_id"`
	MetaAdsetID    *string        `json:"meta_adset_id"`
	MetaAdID       *string        `json:"meta_ad_id"`
	BudgetDaily    *float64       `json:"budget_daily"`
	BudgetTotal    *float64       `json:"budget_total"`
	Currency       *string        `json:"currency"`
	TargetAudience map[string]any `json:"target_audience"`
	FunnelID       *uuid.UUID     `json:"funnel_id"`
	StartDate      *time.Time     `json:"start_date"`
	EndDate        *time.Time     `json:"end_date"`
	Metrics        map[string]any `json:"metrics"`
	Metadata       map[string]any `json:"metadata"`
}

// Create handles POST /api/v1/campaigns
// @Summary Create a new campaign
// @Description Creates a marketing campaign (Meta, Google, or manual)
// @Tags Campaigns
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createCampaignRequest true "Campaign data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 500 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /campaigns [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req createCampaignRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.Name == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "name is required", nil)
		return
	}

	if req.Channel != nil && *req.Channel != "email" && *req.Channel != "sms" && *req.Channel != "whatsapp" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_channel", "channel must be email, sms or whatsapp", nil)
		return
	}
	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:       tenantID,
		Name:           req.Name,
		CampaignType:   req.CampaignType,
		Channel:        req.Channel,
		Platform:       req.Platform,
		MetaCampaignID: req.MetaCampaignID,
		MetaAdsetID:    req.MetaAdsetID,
		MetaAdID:       req.MetaAdID,
		BudgetDaily:    req.BudgetDaily,
		BudgetTotal:    req.BudgetTotal,
		Currency:       req.Currency,
		TargetAudience: req.TargetAudience,
		FunnelID:       req.FunnelID,
		StartDate:      req.StartDate,
		EndDate:        req.EndDate,
		Metrics:        req.Metrics,
		Metadata:       req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to create campaign", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create campaign", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/campaigns
// @Summary List campaigns
// @Description Returns paginated campaigns with optional filters
// @Tags Campaigns
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param status query string false "Filter by status: draft, active, paused, completed, failed"
// @Param campaign_type query string false "Filter by type: lead_gen, retargeting, recruitment, re_engagement"
// @Param platform query string false "Filter by platform: meta, manual, google"
// @Param limit query int false "Page size (default 20, max 100)"
// @Param offset query int false "Offset for pagination"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /campaigns [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	status := r.URL.Query().Get("status")
	campaignType := r.URL.Query().Get("campaign_type")
	platform := r.URL.Query().Get("platform")

	p := pagination.Parse(r)
	input := ListInput{
		TenantID: tenantID,
		Limit:    p.Limit,
		Offset:   p.Offset,
	}
	if status != "" {
		input.Status = &status
	}
	if campaignType != "" {
		input.CampaignType = &campaignType
	}
	if platform != "" {
		input.Platform = &platform
	}

	results, total, err := h.service.List(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to list campaigns", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list campaigns", nil)
		return
	}

	ids := make([]uuid.UUID, 0, len(results))
	for _, c := range results {
		ids = append(ids, c.ID)
	}
	counts, err := h.service.LeadCounts(r.Context(), tenantID, ids)
	if err != nil {
		h.logger.Warn("campaign lead counts failed", zap.Error(err))
		counts = map[uuid.UUID]int{}
	}
	// The page envelope plus lead_counts keyed by campaign id.
	resp := pagination.NewResponse(results, total, p)
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"data": resp.Data, "total": resp.Total, "page": resp.Page, "limit": resp.Limit, "has_more": resp.HasMore,
		"lead_counts": counts,
	})
}

// Get handles GET /api/v1/campaigns/{id}
// @Summary Get a campaign by ID
// @Tags Campaigns
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /campaigns/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	campaignID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid campaign ID", nil)
		return
	}

	result, err := h.service.Get(r.Context(), tenantID, campaignID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "campaign not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/campaigns/{id}
// @Summary Update a campaign
// @Tags Campaigns
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Param request body updateCampaignRequest true "Fields to update"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /campaigns/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	campaignID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid campaign ID", nil)
		return
	}

	var req updateCampaignRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.Channel != nil && *req.Channel != "email" && *req.Channel != "sms" && *req.Channel != "whatsapp" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_channel", "channel must be email, sms or whatsapp", nil)
		return
	}
	result, err := h.service.Update(r.Context(), tenantID, campaignID, UpdateInput{
		Name:           req.Name,
		CampaignType:   req.CampaignType,
		Channel:        req.Channel,
		Platform:       req.Platform,
		Status:         req.Status,
		MetaCampaignID: req.MetaCampaignID,
		MetaAdsetID:    req.MetaAdsetID,
		MetaAdID:       req.MetaAdID,
		BudgetDaily:    req.BudgetDaily,
		BudgetTotal:    req.BudgetTotal,
		Currency:       req.Currency,
		TargetAudience: req.TargetAudience,
		FunnelID:       req.FunnelID,
		StartDate:      req.StartDate,
		EndDate:        req.EndDate,
		Metrics:        req.Metrics,
		Metadata:       req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to update campaign", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update campaign", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/campaigns/{id}
// @Summary Delete a campaign (marks as completed)
// @Tags Campaigns
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /campaigns/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	campaignID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid campaign ID", nil)
		return
	}

	if err := h.service.Delete(r.Context(), tenantID, campaignID); err != nil {
		h.logger.Error("failed to delete campaign", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete campaign", nil)
		return
	}

	audit.Tenant(r.Context(), h.service.client, h.logger, tenantID, "campaign.deleted", "campaign", &campaignID, nil)
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// Pause handles POST /api/v1/campaigns/{id}/pause
// @Summary Pause a campaign
// @Description Sets campaign status to paused
// @Tags Campaigns
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /campaigns/{id}/pause [post]
func (h *Handler) Pause(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	campaignID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid campaign ID", nil)
		return
	}

	result, err := h.service.Pause(r.Context(), tenantID, campaignID)
	if err != nil {
		h.logger.Error("failed to pause campaign", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "pause_failed", "failed to pause campaign", nil)
		return
	}

	audit.Tenant(r.Context(), h.service.client, h.logger, tenantID, "campaign.paused", "campaign", &campaignID, nil)
	httpapi.JSON(w, http.StatusOK, result)
}

// Resume handles POST /api/v1/campaigns/{id}/resume
// @Summary Resume a campaign
// @Description Sets campaign status to active
// @Tags Campaigns
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /campaigns/{id}/resume [post]
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	campaignID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid campaign ID", nil)
		return
	}

	result, err := h.service.Resume(r.Context(), tenantID, campaignID)
	if err != nil {
		h.logger.Error("failed to resume campaign", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "resume_failed", "failed to resume campaign", nil)
		return
	}

	audit.Tenant(r.Context(), h.service.client, h.logger, tenantID, "campaign.resumed", "campaign", &campaignID, nil)
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

