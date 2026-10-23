package creative

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves the Creative Studio routes.
type Handler struct {
	svc     *Service
	reviews *Reviews
	logger  *zap.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, reviews *Reviews, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, reviews: reviews, logger: logger.Named("creative.handler")}
}

func tenantOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}

func idParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return uuid.Nil, false
	}
	return id, true
}

// userOf is the signed-in user, when the token's subject is a UUID.
func userOf(r *http.Request) *uuid.UUID {
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil {
		if id, err := uuid.Parse(c.Subject); err == nil {
			return &id
		}
	}
	return nil
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case ent.IsNotFound(err):
		httpapi.Error(w, http.StatusNotFound, "not_found", "not found", nil)
	case errors.Is(err, ErrInvalid):
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
	case errors.Is(err, ErrConflict):
		httpapi.Error(w, http.StatusConflict, "conflict", err.Error(), nil)
	case errors.Is(err, ErrDraftLimit):
		httpapi.Error(w, http.StatusTooManyRequests, "draft_limit", "too many drafts are waiting; approve or discard some first", nil)
	default:
		h.logger.Error("creative request failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	}
}

// ListBriefs handles GET /api/v1/creative/briefs.
// @Summary List creative briefs
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param status query string false "active (default) or archived"
// @Param page query int false "Page"
// @Param page_size query int false "Page size"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /creative/briefs [get]
func (h *Handler) ListBriefs(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	p := pagination.Parse(r)
	rows, total, err := h.svc.ListBriefs(r.Context(), tid, r.URL.Query().Get("status"), p.Limit, p.Offset)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(rows, total, p))
}

// CreateBrief handles POST /api/v1/creative/briefs.
// @Summary Create a creative brief
// @Tags Creative
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body BriefInput true "Brief"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/briefs [post]
func (h *Handler) CreateBrief(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	var in BriefInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	b, err := h.svc.CreateBrief(r.Context(), tid, userOf(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, b)
}

// GetBrief handles GET /api/v1/creative/briefs/{id}: the brief with its latest
// 50 versions and their files.
// @Summary Get a creative brief with its versions
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Brief UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/briefs/{id} [get]
func (h *Handler) GetBrief(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	b, err := h.svc.GetBrief(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, b)
}

// UpdateBrief handles PUT /api/v1/creative/briefs/{id}.
// @Summary Update a creative brief
// @Tags Creative
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Brief UUID"
// @Param request body BriefInput true "Fields to change"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/briefs/{id} [put]
func (h *Handler) UpdateBrief(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in BriefInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	b, err := h.svc.UpdateBrief(r.Context(), tid, id, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, b)
}

// CreateVersion handles POST /api/v1/creative/briefs/{id}/versions: queues a
// poster. The worker makes it in about a minute; poll GET /creative/versions/{id}.
// @Summary Queue a poster version of a brief
// @Tags Creative
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Brief UUID"
// @Param request body VersionInput true "Poster request"
// @Success 202 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 429 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/briefs/{id}/versions [post]
func (h *Handler) CreateVersion(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in VersionInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	v, err := h.svc.CreateVersion(r.Context(), tid, id, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusAccepted, v)
}

// GetVersion handles GET /api/v1/creative/versions/{id}.
// @Summary Get a creative version with its files
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Version UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id} [get]
func (h *Handler) GetVersion(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	v, err := h.svc.GetVersion(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, v)
}

// Approve handles POST /api/v1/creative/versions/{id}/approve.
// @Summary Approve a draft: its files are kept
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Version UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/approve [post]
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Approve(r.Context(), tid, id, userOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, v)
}

// Discard handles POST /api/v1/creative/versions/{id}/discard.
// @Summary Discard a draft: its files are deleted after a day
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Version UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/discard [post]
func (h *Handler) Discard(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Discard(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, v)
}

// reviewLinkRequest sets how long a review link lasts.
type reviewLinkRequest struct {
	Hours int `json:"hours"`
}

// CreateReviewLink handles POST /api/v1/creative/versions/{id}/review-link.
// @Summary Make a review link for a draft (default 7 days, max 30)
// @Tags Creative
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Version UUID"
// @Param request body reviewLinkRequest false "Lifetime in hours"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/review-link [post]
func (h *Handler) CreateReviewLink(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var req reviewLinkRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
			return
		}
	}
	token, exp, err := h.reviews.Link(r.Context(), tid, id, time.Duration(req.Hours)*time.Hour)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			httpapi.Error(w, http.StatusConflict, "conflict", "only a ready or approved version can be shared", nil)
			return
		}
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"path": "/r/" + token, "token": token, "expires_at": exp})
}

// RevokeReviewLinks handles DELETE /api/v1/creative/versions/{id}/review-link.
// @Summary Revoke every review link of a draft
// @Tags Creative
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Version UUID"
// @Success 204
// @Security bearerAuth
// @Router /creative/versions/{id}/review-link [delete]
func (h *Handler) RevokeReviewLinks(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if err := h.reviews.Revoke(r.Context(), tid, id); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// OpenReview handles the public GET /r/{token}: a 302 to the draft's image.
// Every failure is the same 404 so a token reveals nothing.
func (h *Handler) OpenReview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	url, err := h.reviews.Open(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		http.Error(w, "This review link has expired or was turned off.", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}
