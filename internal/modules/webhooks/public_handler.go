package webhooks

import (
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/funnels"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// PublicHandler handles public endpoints that require no authentication.
type PublicHandler struct {
	entClient     *ent.Client
	funnelService *funnels.Service
	leadService   *leads.Service
	logger        *zap.Logger
}

// NewPublicHandler creates a new public handler.
func NewPublicHandler(entClient *ent.Client, funnelService *funnels.Service, leadService *leads.Service, logger *zap.Logger) *PublicHandler {
	return &PublicHandler{
		entClient:     entClient,
		funnelService: funnelService,
		leadService:   leadService,
		logger:        logger.Named("public"),
	}
}

// GetProfile handles GET /api/v1/public/profile/{slug}
// @Summary Get tenant profile by slug (public)
// @Description Returns public tenant profile data without authentication
// @Tags Public
// @Produce json
// @Param slug path string true "Tenant URL slug"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/profile/{slug} [get]
func (h *PublicHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return
	}

	t, err := h.entClient.Tenant.Query().
		Where(
			tenant.SlugEQ(slug),
			tenant.StatusEQ("active"),
		).
		Only(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "profile not found", nil)
		return
	}

	// Return only public-safe fields
	profile := map[string]any{
		"id":            t.ID,
		"name":          t.Name,
		"slug":          t.Slug,
		"business_type": t.BusinessType,
	}
	if t.Country != nil {
		profile["country"] = *t.Country
	}
	if t.WhatsappNumber != nil {
		profile["whatsapp_number"] = *t.WhatsappNumber
	}
	if t.FboDistribID != nil {
		profile["fbo_distrib_id"] = *t.FboDistribID
	}
	if t.Metadata != nil {
		profile["metadata"] = t.Metadata
	}

	httpapi.JSON(w, http.StatusOK, profile)
}

// GetFunnel handles GET /api/v1/public/funnels/{slug}
// @Summary Get published funnel by slug (public)
// @Description Returns published funnel data without authentication
// @Tags Public
// @Produce json
// @Param slug path string true "Funnel URL slug"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/funnels/{slug} [get]
func (h *PublicHandler) GetFunnel(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return
	}

	result, err := h.funnelService.GetBySlug(r.Context(), slug)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "funnel not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

type publicLeadInput struct {
	Email       string         `json:"email"`
	Phone       string         `json:"phone"`
	FirstName   string         `json:"first_name"`
	LastName    string         `json:"last_name"`
	Source      string         `json:"source"`
	Interest    string         `json:"interest"`
	LeadType    string         `json:"lead_type"`
	UTMSource   string         `json:"utm_source"`
	UTMMedium   string         `json:"utm_medium"`
	UTMCampaign string         `json:"utm_campaign"`
	FunnelSlug  string         `json:"funnel_slug"`
	Answers     map[string]any `json:"answers"`
}

// PublicCreateLead handles POST /api/v1/public/leads/{slug}
// @Summary Public lead capture
// @Description Unauthenticated lead capture for the tenant identified by URL slug. Used by the embeddable chat widget and public funnels. Publishes `lead.created` to kick off the nurture loop.
// @Tags Public
// @Accept json
// @Produce json
// @Param slug path string true "Tenant URL slug"
// @Param request body publicLeadInput true "Lead data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/leads/{slug} [post]
func (h *PublicHandler) PublicCreateLead(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return
	}

	// Resolve tenant (active tenants only).
	t, err := h.entClient.Tenant.Query().
		Where(tenant.SlugEQ(slug), tenant.StatusEQ("active")).
		Only(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "tenant_not_found", "no tenant for slug", nil)
		return
	}

	var in publicLeadInput
	if err := httpapi.DecodeJSON(r, &in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid JSON body", nil)
		return
	}
	if in.Email == "" && in.Phone == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_contact", "email or phone is required", nil)
		return
	}

	source := in.Source
	if source == "" {
		source = "widget"
	}
	leadType := in.LeadType
	if leadType == "" {
		leadType = "customer"
	}

	input := leads.CreateInput{
		TenantID:             t.ID,
		Source:               source,
		LeadType:             leadType,
		QualificationAnswers: in.Answers,
		Metadata: map[string]any{
			"captured_via":    "public_endpoint",
			"captured_at":     time.Now().UTC().Format(time.RFC3339),
			"remote_addr":     r.RemoteAddr,
			"user_agent":      r.UserAgent(),
			"referer":         r.Referer(),
			"tenant_slug":     slug,
			"funnel_slug":     in.FunnelSlug,
		},
	}
	if in.Email != "" {
		e := in.Email
		input.Email = &e
	}
	if in.Phone != "" {
		p := in.Phone
		input.Phone = &p
	}
	if in.FirstName != "" {
		input.FirstName = &in.FirstName
	}
	if in.LastName != "" {
		input.LastName = &in.LastName
	}
	if in.Interest != "" {
		input.Interest = &in.Interest
	}
	if in.UTMSource != "" {
		input.UTMSource = &in.UTMSource
	}
	if in.UTMMedium != "" {
		input.UTMMedium = &in.UTMMedium
	}
	if in.UTMCampaign != "" {
		input.UTMCampaign = &in.UTMCampaign
	}

	lead, err := h.leadService.Create(r.Context(), input)
	if err != nil {
		h.logger.Error("public lead create failed",
			zap.String("slug", slug), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "could not create lead", nil)
		return
	}

	// Lead creation already emits `lead.created` via the service's outbox hook;
	// marketflow-worker's LeadCreatedHandler picks it up and auto-enrols the lead.
	httpapi.JSON(w, http.StatusCreated, map[string]any{
		"id":         lead.ID,
		"tenant_id":  lead.TenantID,
		"status":     lead.Status,
		"created_at": lead.CreatedAt,
	})
}
