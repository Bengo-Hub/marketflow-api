package creative

import (
	"context"
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
	svc       *Service
	reviews   *Reviews
	offerings *Offerings
	logger    *zap.Logger
	// canApprove gates approval (gate G5: who may approve an ad); nil allows
	// every signed-in tenant user.
	canApprove func(ctx context.Context, tenantID uuid.UUID) bool
	// clips settles motion poster jobs reported by marketflow-ai's callback.
	clips *Renderer
}

// WithClips turns on the clip callback; r needs only the database here.
func (h *Handler) WithClips(r *Renderer) *Handler {
	h.clips = r
	return h
}

// ApprovalRoles are the tenant roles that may approve an ad: the admin and the
// business owner. Viewers may look and share review links, not approve.
var ApprovalRoles = []string{"marketflow_admin", "fbo_owner"}

// WithApprover sets who may approve; see ApprovalRoles.
func (h *Handler) WithApprover(check func(ctx context.Context, tenantID uuid.UUID) bool) *Handler {
	h.canApprove = check
	return h
}

// NewHandler builds the handler. offerings may be nil (the picker answers 503).
func NewHandler(svc *Service, reviews *Reviews, offerings *Offerings, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, reviews: reviews, offerings: offerings, logger: logger.Named("creative.handler")}
}

// SearchOfferings handles GET /api/v1/creative/offerings?q=: sellable items from
// the tenant's catalogue for a brief's offering. 503 when the catalogue is not
// reachable; the UI then lets the user describe the offering as free text.
// @Summary Search the tenant's catalogue for a brief's offering
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param q query string false "Name or SKU"
// @Success 200 {object} map[string]interface{}
// @Failure 503 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/offerings [get]
func (h *Handler) SearchOfferings(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	items, err := h.offerings.Search(r.Context(), tid, r.URL.Query().Get("q"), 20)
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "catalogue_unavailable", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": items})
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
	case errors.Is(err, ErrDailyCap):
		httpapi.Error(w, http.StatusTooManyRequests, "daily_render_limit", "today's poster limit is reached; text edits still work, new posters again tomorrow", nil)
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
// @Param limit query int false "Page size"
// @Param page query int false "Page, from 1 (or use offset)"
// @Param offset query int false "Rows to skip, when page is not given"
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
// @Summary Approve a draft: its files are kept and the brief's other drafts are superseded
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
	if h.canApprove != nil && !h.canApprove(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "approval_not_allowed", "only the business owner or an admin can approve an ad", nil)
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
// @Summary Discard a draft: the next hourly purge deletes its files
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

// Revise handles POST /api/v1/creative/versions/{id}/revise: a text-only
// revision over the same scene. Free; it is made in a few seconds.
// @Summary Revise a poster's words (free, same image)
// @Tags Creative
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Source version UUID"
// @Param request body ReviseInput true "Words to change"
// @Success 202 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/revise [post]
func (h *Handler) Revise(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in ReviseInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	v, err := h.svc.Revise(r.Context(), tid, id, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusAccepted, v)
}

// CreateMotion handles POST /api/v1/creative/versions/{id}/motion: queues a
// motion poster of a finished poster. A clip takes a few minutes; the version
// turns ready when marketflow-ai reports it done.
// @Summary Make a motion poster (a short clip) of a finished poster
// @Tags Creative
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Source poster version UUID"
// @Param request body MotionInput false "Motion, length and tier"
// @Success 202 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 429 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/motion [post]
func (h *Handler) CreateMotion(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var in MotionInput
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
			return
		}
	}
	v, err := h.svc.CreateMotion(r.Context(), tid, id, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusAccepted, v)
}

// ClipCallback handles the S2S POST /api/v1/internal/creative/clips/{id}:
// marketflow-ai reporting a finished or failed clip job (service key only).
// A 5xx makes it try again; a report for a settled version is a no-op 204.
func (h *Handler) ClipCallback(w http.ResponseWriter, r *http.Request) {
	if h.clips == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "unavailable", "clips are not handled here", nil)
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var job ClipJob
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&job); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	if err := h.clips.SettleClip(r.Context(), id, job); err != nil {
		if ent.IsNotFound(err) {
			httpapi.Error(w, http.StatusNotFound, "not_found", "no video version with this id", nil)
			return
		}
		h.logger.Error("clip callback not recorded", zap.String("version_id", id.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "internal_error", "not recorded; try again", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Extend handles POST /api/v1/creative/versions/{id}/extend.
// @Summary Keep a draft for 7 days from when it was made (once)
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Version UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/extend [post]
func (h *Handler) Extend(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Extend(r.Context(), tid, id)
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
// @Summary Make a review link for a draft (default 72 hours, max 7 days)
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
// Every failure is the same 410 so a token reveals nothing.
func (h *Handler) OpenReview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	url, err := h.reviews.Open(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		http.Error(w, "This review link has expired or was turned off.", http.StatusGone)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}
