package brandimages

import (
	"errors"
	"net/http"
	"strings"
	"time"

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
	Images []View `json:"images"`
	// Screenshots are the business's own app screens, with their own limits (kinds.go).
	Screenshots        []View `json:"screenshots"`
	MaxScreens         int    `json:"max_screens"`
	MinScreenShortSide int    `json:"min_screen_short_side"`

	Min         int    `json:"min"`
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
	reply := listReply{Images: []View{}, Screenshots: []View{}, Min: cfg.Min, Max: cfg.Max, MaxBytes: cfg.MaxBytes,
		MinShortSide: cfg.MinShortSide, MaxScreens: cfg.MaxScreens, MinScreenShortSide: cfg.MinScreenSide}
	for _, v := range images {
		if v.Kind == KindScreenshot {
			reply.Screenshots = append(reply.Screenshots, v)
		} else {
			reply.Images = append(reply.Images, v)
		}
	}
	httpapi.JSON(w, http.StatusOK, reply)
}

// Upload handles POST /api/v1/tenant/brand-images (multipart field "file").
// @Summary Upload a brand image (JPEG or PNG, at least 1024 px on the short side)
// @Tags BrandImages
// @Accept multipart/form-data
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param file formData file true "JPEG or PNG, up to 2 MB"
// @Param kind query string false "screenshot for a screen of the business's app"
// @Param device query string false "laptop, tablet, pos or phone; empty goes by the picture's shape"
// @Param no_customer_data query bool false "must be true for a screenshot: it shows no customer's data"
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
	v, err := h.svc.AddAs(r.Context(), tid, up, uploadKind(r))
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

// ImportSite handles POST /api/v1/tenant/brand-images/import-site.
// @Summary Link brand images from the business's own website
// @Description Reads the website and storefront on the tenant's account (home page plus its services, about, products, projects and blog pages, as robots.txt allows) and stores up to the maximum number of brand images from the pictures that show the business. Pictures in client, partner or logo strips, the navigation and the footer, pictures shown small, logos and pictures naming another business are skipped. Each added picture says where it was found (from). Pictures already linked are not taken twice. 502 site_unreadable when no page of the site answered.
// @Tags BrandImages
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} ImportResult
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 422 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/brand-images/import-site [post]
func (h *Handler) ImportSite(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	if !h.svc.SiteImportEnabled() {
		httpapi.Error(w, http.StatusServiceUnavailable, "import_unavailable", "Importing from a website is not configured", nil)
		return
	}
	// Reading a website takes longer than the server's usual write timeout (10 s by default),
	// which used to cut the answer off and leave the portal with no reply at all. This request
	// gets the read window plus time to store, still inside the gateway's 60 s.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(h.svc.Config().Site.ReadWindow + 15*time.Second))
	res, err := h.svc.ImportFromSite(r.Context(), tid)
	if errors.Is(err, ErrNoSite) {
		httpapi.Error(w, http.StatusUnprocessableEntity, "no_website",
			"Add your website to your business details first, then try again", nil)
		return
	}
	if errors.Is(err, ErrSiteUnreadable) {
		h.logger.Info("brand images: website unreadable", zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "site_unreadable",
			"Your website did not answer just now. Try again in a few minutes, or upload your pictures.",
			map[string]any{"sites": res.Sites})
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, res)
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	cfg := h.svc.Config()
	switch {
	case errors.Is(err, ErrNotFound) || ent.IsNotFound(err):
		httpapi.Error(w, http.StatusNotFound, "not_found", "brand image not found", nil)
	case errors.Is(err, ErrCustomerData):
		httpapi.Error(w, http.StatusUnprocessableEntity, "confirm_no_customer_data", err.Error(), nil)
	case errors.Is(err, ErrInvalidKind):
		httpapi.Error(w, http.StatusBadRequest, "invalid_kind", err.Error(), nil)
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
