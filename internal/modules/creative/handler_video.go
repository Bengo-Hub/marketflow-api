package creative

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"go.uber.org/zap"
)

// MakeStoryboard handles POST /api/v1/creative/versions/{id}/storyboard: queues the
// storyboard of a video ad script waiting for review (one still per scene; a presenter ad's
// actor is drawn once per brief). About 1 credit, free when only the owner's photos are used.
// @Summary Make a video ad's storyboard
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Video ad version UUID"
// @Success 202 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/storyboard [post]
func (h *Handler) MakeStoryboard(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if !h.requireBrandImages(w, r, tid) {
		return
	}
	// An optional body {"redraw_actor": true, "actor_prompt": "..."} draws a new presenter.
	var opt StoryboardOptions
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&opt); err != nil && !errors.Is(err, io.EOF) {
			httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
			return
		}
	}
	v, err := h.svc.MakeStoryboardWith(r.Context(), tid, id, opt)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusAccepted, v)
}

// ApproveStoryboard handles POST /api/v1/creative/versions/{id}/storyboard/approve: the
// owner accepts the stills; a presenter ad can then be rendered. Editing the script after
// this asks for the approval again.
// @Summary Approve a video ad's storyboard
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Video ad version UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/storyboard/approve [post]
func (h *Handler) ApproveStoryboard(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	// With {"plan_fingerprint": "..."} (from the quote) the generation plan is approved too: the
	// exact prompts and first frames the quote showed. The render needs that approval.
	var body struct {
		PlanFingerprint string `json:"plan_fingerprint"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
			return
		}
	}
	if body.PlanFingerprint == "" {
		v, err := h.svc.ApproveStoryboard(r.Context(), tid, id, userOf(r))
		if err != nil {
			h.fail(w, err)
			return
		}
		httpapi.JSON(w, http.StatusOK, v)
		return
	}
	v, err := h.svc.version(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	// The plan as the providers will get it, kept with the approval; a scene still waiting for its
	// storyboard still cannot be approved.
	var scenes []any
	if h.jobs != nil {
		if data, status, err := h.jobs.QuoteVideoAd(r.Context(), v); err == nil && status == http.StatusOK {
			if scene := missingStill(data); scene != "" {
				h.fail(w, fmt.Errorf("%w: scene %s needs a storyboard still; make the storyboard first", ErrConflict, scene))
				return
			}
			scenes = planScenes(data)
		}
	}
	v, err = h.svc.ApproveStoryboardAndPlan(r.Context(), tid, id, userOf(r), body.PlanFingerprint, scenes)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, v)
}

// ReviseVideoAd handles POST /api/v1/creative/versions/{id}/revision: a new script of a
// finished video ad, waiting for the owner's scene edits. Its render pays only for the
// scenes that changed.
// @Summary Change a finished video ad scene by scene
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Finished video ad version UUID"
// @Success 201 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 429 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/revision [post]
func (h *Handler) ReviseVideoAd(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if !h.requireBrandImages(w, r, tid) {
		return
	}
	v, err := h.svc.ReviseVideoAd(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, v)
}

// QuoteVideoAd handles GET /api/v1/creative/versions/{id}/quote: what rendering this video
// ad would cost now, scene by scene (presenter and clip scenes from the providers' cached
// estimates, unchanged scenes free), the credit balance and, when short, the bundle to buy.
// Nothing is charged. Cache-Control: no-store, because the balance changes.
// @Summary Quote a video ad's render
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Video ad version UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 503 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/quote [get]
func (h *Handler) QuoteVideoAd(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if h.jobs == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "unavailable", "video is not available right now", nil)
		return
	}
	v, err := h.svc.version(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	data, status, err := h.jobs.QuoteVideoAd(r.Context(), v)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			h.fail(w, err)
			return
		}
		h.logger.Warn("video ad quote failed", zap.String("version_id", id.String()), zap.Error(err))
		httpapi.Error(w, http.StatusServiceUnavailable, "quote_unavailable", "the price could not be worked out right now", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if status >= 500 {
		status = http.StatusServiceUnavailable
	}
	if status == http.StatusOK {
		data = withPlanFingerprint(data, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// RecomposeEndCardPreview handles POST /api/v1/creative/versions/{id}/end-card-preview: the end
// card preview made again over the storyboard's own end card still, with the current words,
// contacts and logo plate. Nothing is redrawn or charged and an approval stands.
// @Summary Recompose a video ad's end card preview
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Video ad version UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 503 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/end-card-preview [post]
func (h *Handler) RecomposeEndCardPreview(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if h.jobs == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "unavailable", "video is not available right now", nil)
		return
	}
	v, err := h.svc.version(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	v, err = h.jobs.RecomposeEndCardPreview(r.Context(), v)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			h.fail(w, err)
			return
		}
		h.logger.Warn("end card preview not recomposed", zap.String("version_id", id.String()), zap.Error(err))
		httpapi.Error(w, http.StatusServiceUnavailable, "preview_unavailable", "the end card preview could not be made right now", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, v)
}
