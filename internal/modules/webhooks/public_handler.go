package webhooks

import (
	"net/http"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/funnels"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// PublicHandler handles public endpoints that require no authentication.
type PublicHandler struct {
	entClient     *ent.Client
	funnelService *funnels.Service
	logger        *zap.Logger
}

// NewPublicHandler creates a new public handler.
func NewPublicHandler(entClient *ent.Client, funnelService *funnels.Service, logger *zap.Logger) *PublicHandler {
	return &PublicHandler{
		entClient:     entClient,
		funnelService: funnelService,
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
