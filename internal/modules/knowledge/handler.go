package knowledge

import (
	"fmt"
	"net/http"

	"github.com/Bengo-Hub/pagination"
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
// @Summary Create a knowledge base entry
// @Description Creates a product, FAQ, compliance, or business opportunity KB entry for RAG
// @Tags KnowledgeBase
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createEntryRequest true "KB entry data"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /knowledge/entries [post]
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
// @Summary List knowledge base entries
// @Tags KnowledgeBase
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param category query string false "Filter: product, faq, compliance, business_opportunity, custom"
// @Param limit query int false "Page size"
// @Param offset query int false "Offset"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /knowledge/entries [get]
func (h *Handler) ListEntries(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	category := r.URL.Query().Get("category")
	p := pagination.Parse(r)
	input := ListEntriesInput{
		TenantID: tenantID,
		Limit:    p.Limit,
		Offset:   p.Offset,
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

	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
}

// GetEntry handles GET /api/v1/knowledge/entries/{id}
// @Summary Get a knowledge base entry
// @Tags KnowledgeBase
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Entry UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /knowledge/entries/{id} [get]
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
// @Summary Update a knowledge base entry
// @Description Updates content and auto-marks for re-embedding if content changed
// @Tags KnowledgeBase
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Entry UUID"
// @Param request body updateEntryRequest true "Fields to update"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /knowledge/entries/{id} [put]
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
// @Summary Delete a knowledge base entry
// @Tags KnowledgeBase
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Entry UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /knowledge/entries/{id} [delete]
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

// ImportCSV handles POST /api/v1/knowledge/entries/import
// @Summary Bulk-import knowledge base entries from a CSV file
// @Description Accepts multipart/form-data with a 'file' field (CSV). Columns: category, title, content, product_code, product_price, product_cc, product_image_url, tags (pipe-separated)
// @Tags KnowledgeBase
// @Accept multipart/form-data
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param file formData file true "CSV file"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /knowledge/entries/import [post]
func (h *Handler) ImportCSV(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	if err := r.ParseMultipartForm(10 << 20); err != nil { // 10 MB limit
		httpapi.Error(w, http.StatusBadRequest, "parse_form_failed", "failed to parse multipart form", nil)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "file_missing", "form field 'file' is required", nil)
		return
	}
	defer file.Close()

	result, err := h.service.BulkImportCSV(r.Context(), tenantID, file)
	if err != nil {
		h.logger.Error("CSV import failed", zap.Error(err))
		httpapi.Error(w, http.StatusBadRequest, "import_failed", err.Error(), nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

