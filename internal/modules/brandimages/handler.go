package brandimages

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Handler serves the brand image routes under the authenticated tenant API.
type Handler struct {
	svc    *Service
	logger *zap.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, logger: logger.Named("brandimages.handler")}
}

// listReply is the list with the limits, so the portal shows "3 of 5" from the server's numbers.
type listReply struct {
	Images       []View `json:"images"`
	Min          int    `json:"min"`
	Max          int    `json:"max"`
	MaxBytes     int64  `json:"max_bytes"`
	MinShortSide int    `json:"min_short_side"`
}

// tenantOf reads the tenant the protected group's tenant check has already verified against the
// token (httpware TenantV2 in the router).
func tenantOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}

// List handles GET /api/v1/tenant/brand-images.
// @Summary List the tenant's brand images with short-lived links
// @Tags BrandImages
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} listReply
// @Security bearerAuth
// @Router /tenant/brand-images [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	images, err := h.svc.List(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	cfg := h.svc.Config()
	httpapi.JSON(w, http.StatusOK, listReply{Images: images, Min: cfg.Min, Max: cfg.Max,
		MaxBytes: cfg.MaxBytes, MinShortSide: cfg.MinShortSide})
}

// Upload handles POST /api/v1/tenant/brand-images (multipart field "file").
// @Summary Upload a brand image (JPEG or PNG, at least 1024 px on the short side)
// @Tags BrandImages
// @Accept multipart/form-data
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param file formData file true "JPEG or PNG, up to 2 MB"
// @Success 201 {object} View
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/brand-images [post]
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	if !h.svc.store.Enabled() {
		httpapi.Error(w, http.StatusServiceUnavailable, "storage_unavailable", "File storage is not configured", nil)
		return
	}
	up, err := objectstore.ReadUpload(w, r, "file", MediaType)
	if err != nil {
		if objectstore.IsUploadError(err) {
			httpapi.Error(w, http.StatusBadRequest, "invalid_file", err.Error(), nil)
			return
		}
		h.logger.Error("brand image upload not read", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "internal", "Failed to read the upload", nil)
		return
	}
	v, err := h.svc.Add(r.Context(), tid, up)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, v)
}

// Delete handles DELETE /api/v1/tenant/brand-images/{id}.
// @Summary Delete a brand image
// @Tags BrandImages
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Image id"
// @Success 204
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/brand-images/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), tid, chi.URLParam(r, "id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	cfg := h.svc.Config()
	switch {
	case errors.Is(err, ErrNotFound) || ent.IsNotFound(err):
		httpapi.Error(w, http.StatusNotFound, "not_found", "brand image not found", nil)
	case errors.Is(err, ErrFull):
		httpapi.Error(w, http.StatusConflict, "limit_reached", "you already have the most brand images allowed; delete one first",
			map[string]any{"max": cfg.Max})
	case errors.Is(err, ErrTooSmall):
		msg := strings.TrimPrefix(err.Error(), ErrTooSmall.Error()+": ")
		httpapi.Error(w, http.StatusBadRequest, "image_too_small", msg, map[string]any{"min_short_side": cfg.MinShortSide})
	case errors.Is(err, ErrUnavailable):
		httpapi.Error(w, http.StatusServiceUnavailable, "storage_unavailable", "File storage is not configured", nil)
	case errors.Is(err, errStore):
		h.logger.Error("brand image not stored", zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "storage_unavailable", "File storage is unavailable", nil)
	default:
		h.logger.Error("brand images", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "internal", "Something went wrong; nothing was changed", nil)
	}
}
