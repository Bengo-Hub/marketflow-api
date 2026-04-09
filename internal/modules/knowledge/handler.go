package knowledge

import (
	"fmt"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for knowledge base entries.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new knowledge base handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("knowledge.handler"),
	}
}

type createEntryRequest struct {
	Category        string         `json:"category"`
	Title           string         `json:"title"`
	Content         string         `json:"content"`
	ProductCode     *string        `json:"product_code"`
	ProductPrice    *float64       `json:"product_price"`
	ProductCC       *float64       `json:"product_cc"`
	ProductImageURL *string        `json:"product_image_url"`
	Tags            []string       `json:"tags"`
	Metadata        map[string]any `json:"metadata"`
}

type updateEntryRequest struct {
	Category        *string        `json:"category"`
	Title           *string        `json:"title"`
	Content         *string        `json:"content"`
	ProductCode     *string        `json:"product_code"`
	ProductPrice    *float64       `json:"product_price"`
	ProductCC       *float64       `json:"product_cc"`
	ProductImageURL *string        `json:"product_image_url"`
	Tags            []string       `json:"tags"`
	Metadata        map[string]any `json:"metadata"`
}

// CreateEntry handles POST /api/v1/knowledge/entries
func (h *Handler) CreateEntry(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req createEntryRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.Title == "" || req.Content == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "title and content are required", nil)
		return
	}
	if req.Category == "" {
		req.Category = "product"
	}

	result, err := h.service.CreateEntry(r.Context(), CreateEntryInput{
		TenantID:        tenantID,
		Category:        req.Category,
		Title:           req.Title,
		Content:         req.Content,
		ProductCode:     req.ProductCode,
		ProductPrice:    req.ProductPrice,
		ProductCC:       req.ProductCC,
		ProductImageURL: req.ProductImageURL,
		Tags:            req.Tags,
		Metadata:        req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to create KB entry", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create KB entry", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, result)
}

// ListEntries handles GET /api/v1/knowledge/entries
func (h *Handler) ListEntries(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	category := r.URL.Query().Get("category")
	input := ListEntriesInput{
		TenantID: tenantID,
		Limit:    parseIntQuery(r, "limit", 20),
		Offset:   parseIntQuery(r, "offset", 0),
	}
	if category != "" {
		input.Category = &category
	}

	results, total, err := h.service.ListEntries(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to list KB entries", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list KB entries", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"data":  results,
		"total": total,
	})
}

// GetEntry handles GET /api/v1/knowledge/entries/{id}
func (h *Handler) GetEntry(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	entryID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid entry ID", nil)
		return
	}

	result, err := h.service.GetEntry(r.Context(), tenantID, entryID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "KB entry not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// UpdateEntry handles PUT /api/v1/knowledge/entries/{id}
func (h *Handler) UpdateEntry(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	entryID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid entry ID", nil)
		return
	}

	var req updateEntryRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	result, err := h.service.UpdateEntry(r.Context(), tenantID, entryID, UpdateEntryInput{
		Category:        req.Category,
		Title:           req.Title,
		Content:         req.Content,
		ProductCode:     req.ProductCode,
		ProductPrice:    req.ProductPrice,
		ProductCC:       req.ProductCC,
		ProductImageURL: req.ProductImageURL,
		Tags:            req.Tags,
		Metadata:        req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to update KB entry", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update KB entry", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// DeleteEntry handles DELETE /api/v1/knowledge/entries/{id}
func (h *Handler) DeleteEntry(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	entryID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid entry ID", nil)
		return
	}

	if err := h.service.DeleteEntry(r.Context(), tenantID, entryID); err != nil {
		h.logger.Error("failed to delete KB entry", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete KB entry", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

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
