package shortlinks

import (
	"fmt"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for short links.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new short link handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("shortlinks.handler"),
	}
}

type createShortLinkRequest struct {
	TargetURL  string     `json:"target_url"`
	LinkType   string     `json:"link_type"`
	Title      *string    `json:"title"`
	CampaignID *uuid.UUID `json:"campaign_id"`
}

// Create handles POST /api/v1/short-links
// @Summary Create a short link
// @Description Generates a shortened URL that redirects to the target. Tracks clicks.
// @Tags ShortLinks
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createShortLinkRequest true "Short link data"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /short-links [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req createShortLinkRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.TargetURL == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_target", "target_url is required", nil)
		return
	}

	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:   tenantID,
		TargetURL:  req.TargetURL,
		LinkType:   req.LinkType,
		Title:      req.Title,
		CampaignID: req.CampaignID,
	})
	if err != nil {
		h.logger.Error("failed to create short link", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create short link", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, map[string]any{
		"id":        result.ID,
		"code":      result.Code,
		"short_url": h.service.ShortURL(result.Code),
		"target_url": result.TargetURL,
		"link_type": result.LinkType,
	})
}

// List handles GET /api/v1/short-links
// @Summary List short links
// @Tags ShortLinks
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param link_type query string false "Filter: product, category, recruit, profile, funnel, whatsapp, custom"
// @Param limit query int false "Page size"
// @Param offset query int false "Offset"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /short-links [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	linkType := r.URL.Query().Get("link_type")
	var lt *string
	if linkType != "" {
		lt = &linkType
	}

	p := pagination.Parse(r)
	results, total, err := h.service.ListByTenant(r.Context(), tenantID, lt, p.Limit, p.Offset)
	if err != nil {
		h.logger.Error("failed to list short links", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list short links", nil)
		return
	}

	// Enrich with short URLs
	items := make([]map[string]any, len(results))
	for i, sl := range results {
		items[i] = map[string]any{
			"id":              sl.ID,
			"code":            sl.Code,
			"short_url":       h.service.ShortURL(sl.Code),
			"target_url":      sl.TargetURL,
			"link_type":       sl.LinkType,
			"title":           sl.Title,
			"click_count":     sl.ClickCount,
			"last_clicked_at": sl.LastClickedAt,
			"created_at":      sl.CreatedAt,
		}
	}

	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(items, total, p))
}

// Redirect handles GET /s/{code} — public, no auth required.
// Resolves the short code and 302-redirects to the target URL.
func (h *Handler) Redirect(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		http.NotFound(w, r)
		return
	}

	targetURL, err := h.service.Resolve(r.Context(), code)
	if err != nil {
		h.logger.Warn("short link resolve failed", zap.String("code", code), zap.Error(err))
		http.NotFound(w, r)
		return
	}

	http.Redirect(w, r, targetURL, http.StatusFound)
}

func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

