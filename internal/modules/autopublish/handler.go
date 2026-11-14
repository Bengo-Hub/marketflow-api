package autopublish

import (
	"context"
	"errors"
	"net/http"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves the veto-window routes: the dashboard's list of the agents' posts, the signed-in
// veto, and the public cancel link from the heads-up email.
type Handler struct {
	P *Pipeline
	// CanApprove gates the signed-in veto to the roles that approve ads (creative.ApprovalRoles);
	// nil allows every caller the write gate let through.
	CanApprove func(ctx context.Context, tenantID uuid.UUID) bool
	Logger     *zap.Logger
}

// NewHandler builds the handler.
func NewHandler(p *Pipeline, logger *zap.Logger) *Handler {
	return &Handler{P: p, Logger: logger.Named("autopublish.handler")}
}

// Posts handles GET /api/v1/agents/autopilot/posts?agent_id=&state=: the tenant's veto-window
// videos of the last 30 days with their scheduled posts, newest first (at most 50).
// @Summary Ad autopilot posts in the veto window
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param agent_id query string false "Only this agent"
// @Param state query string false "Comma-separated: active, scheduled, held, vetoed, released, failed"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/autopilot/posts [get]
func (h *Handler) Posts(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	var agentID *uuid.UUID
	if s := r.URL.Query().Get("agent_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_request", "agent_id must be a UUID", nil)
			return
		}
		agentID = &id
	}
	var states []string
	if s := r.URL.Query().Get("state"); s != "" {
		for _, x := range strings.Split(s, ",") {
			if x = strings.TrimSpace(x); x != "" {
				states = append(states, x)
			}
		}
	}
	out, err := h.P.Posts(r.Context(), tid, agentID, states)
	if err != nil {
		h.Logger.Error("autopilot posts", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out})
}

// Veto handles POST /api/v1/agents/autopilot/posts/{id}/veto: cancels the agent's post of this
// video before it runs. The approving roles only. 409 too_late when the post already went out.
// @Summary Cancel an ad autopilot post in its veto window
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Video ad version UUID"
// @Success 200 {object} VetoResult
// @Failure 403 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /agents/autopilot/posts/{id}/veto [post]
func (h *Handler) Veto(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	if h.CanApprove != nil && !h.CanApprove(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can cancel a post", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return
	}
	by := "user"
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil && c.Subject != "" {
		by = "user:" + c.Subject
	}
	res, err := h.P.Veto(r.Context(), tid, id, by)
	h.vetoReply(w, res, err)
}

// VetoJob handles POST /api/v1/agents/autopilot/posts/{id}/jobs/{job_id}/veto: cancels the
// agent's post of this video on one account only (owner 2026-11-14). The other accounts' posts
// still go out when the window ends. The approving roles only. 409 too_late when that account's
// post already went out.
// @Summary Cancel one account's ad autopilot post in its veto window
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Video ad version UUID"
// @Param job_id path string true "Publish job UUID of the account's post"
// @Success 200 {object} VetoResult
// @Failure 403 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /agents/autopilot/posts/{id}/jobs/{job_id}/veto [post]
func (h *Handler) VetoJob(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	if h.CanApprove != nil && !h.CanApprove(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can cancel a post", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "job_id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid job id", nil)
		return
	}
	by := "user"
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil && c.Subject != "" {
		by = "user:" + c.Subject
	}
	res, err := h.P.VetoJob(r.Context(), tid, id, jobID, by)
	h.vetoReply(w, res, err)
}

// PublicView handles the public GET /api/v1/public/autopilot/veto/{token}: what the emailed
// cancel link points at, for the confirmation page. Nothing changes.
// @Summary Read the post a cancel link points at
// @Tags Public
// @Produce json
// @Param token path string true "Cancel link token"
// @Success 200 {object} PublicPost
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/autopilot/veto/{token} [get]
func (h *Handler) PublicView(w http.ResponseWriter, r *http.Request) {
	id, err := h.P.Links.Open(chi.URLParam(r, "token"))
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "invalid_link", ErrVetoLink.Error(), nil)
		return
	}
	out, _, err := h.P.PublicView(r.Context(), id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "invalid_link", ErrVetoLink.Error(), nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.JSON(w, http.StatusOK, out)
}

// PublicVeto handles the public POST /api/v1/public/autopilot/veto/{token}: the emailed "Cancel
// this post" link, confirmed on the page. The signed, expiring token is the authority, like a
// Studio review link; the actor recorded is veto_link.
// @Summary Cancel a post from the emailed link
// @Tags Public
// @Produce json
// @Param token path string true "Cancel link token"
// @Success 200 {object} VetoResult
// @Failure 404 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Router /public/autopilot/veto/{token} [post]
func (h *Handler) PublicVeto(w http.ResponseWriter, r *http.Request) {
	id, err := h.P.Links.Open(chi.URLParam(r, "token"))
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "invalid_link", ErrVetoLink.Error(), nil)
		return
	}
	_, tid, err := h.P.PublicView(r.Context(), id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "invalid_link", ErrVetoLink.Error(), nil)
		return
	}
	res, err := h.P.Veto(r.Context(), tid, id, ActorVetoLink)
	h.vetoReply(w, res, err)
}

func (h *Handler) vetoReply(w http.ResponseWriter, res VetoResult, err error) {
	switch {
	case err == nil:
		httpapi.JSON(w, http.StatusOK, res)
	case errors.Is(err, ErrTooLate):
		httpapi.Error(w, http.StatusConflict, "too_late", res.Message, map[string]any{"state": res.State, "jobs": res.Gone})
	case errors.Is(err, ErrNotVetoable):
		msg := res.Message
		if msg == "" {
			msg = err.Error()
		}
		httpapi.Error(w, http.StatusConflict, "not_vetoable", msg, map[string]any{"state": res.State})
	case errors.Is(err, creative.ErrConflict):
		httpapi.Error(w, http.StatusConflict, "conflict", "the video changed just now; try again", nil)
	case ent.IsNotFound(err):
		httpapi.Error(w, http.StatusNotFound, "not_found", "not found", nil)
	default:
		h.Logger.Error("autopilot veto", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	}
}

func tenantOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}
