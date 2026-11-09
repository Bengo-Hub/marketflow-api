package endpoints

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves the platform owner's approval page and marketflow-ai's tool list.
type Handler struct {
	Store   EntStore
	Scanner *Scanner
	Logger  *zap.Logger
}

type serviceList struct {
	Service   string     `json:"service"`
	Endpoints []Endpoint `json:"endpoints"`
}

// List handles GET /api/v1/admin/endpoints (platform owner): every scanned service with its
// candidate, approved and rejected routes.
// @Summary Discovered service endpoints
// @Tags Admin
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/endpoints [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	all, err := h.Store.All(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]serviceList, 0, len(h.Scanner.Bases()))
	for svc := range h.Scanner.Bases() {
		list := all[svc]
		if list == nil {
			list = []Endpoint{}
		}
		out = append(out, serviceList{Service: svc, Endpoints: list})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	httpapi.JSON(w, http.StatusOK, map[string]any{"services": out})
}

// Decide handles PATCH /api/v1/admin/endpoints/{service} (platform owner) with
// {"method","path","status"}: approve, reject or return a route to candidate.
// @Summary Approve or reject a discovered endpoint
// @Tags Admin
// @Param service path string true "Service name"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /admin/endpoints/{service} [patch]
func (h *Handler) Decide(w http.ResponseWriter, r *http.Request) {
	svc := chi.URLParam(r, "service")
	if _, ok := h.Scanner.Bases()[svc]; !ok {
		httpapi.Error(w, http.StatusNotFound, "unknown_service", "no such service", nil)
		return
	}
	var body struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "send method, path and status", nil)
		return
	}
	by := actor(r.Context())
	var decided []Endpoint
	err := h.Store.Update(r.Context(), svc, func(list []Endpoint) ([]Endpoint, error) {
		next, err := Decide(list, body.Method, body.Path, body.Status, by, time.Now().UTC())
		decided = next
		return next, err
	})
	switch {
	case errors.Is(err, ErrUnknownEndpoint):
		httpapi.Error(w, http.StatusNotFound, "unknown_endpoint", err.Error(), nil)
		return
	case errors.Is(err, ErrBadStatus):
		httpapi.Error(w, http.StatusBadRequest, "invalid_status", err.Error(), nil)
		return
	case err != nil:
		h.fail(w, err)
		return
	}
	h.Logger.Info("endpoint decision", zap.String("service", svc), zap.String("path", body.Path),
		zap.String("status", body.Status), zap.String("by", by))
	httpapi.JSON(w, http.StatusOK, serviceList{Service: svc, Endpoints: decided})
}

// Scan handles POST /api/v1/admin/endpoints/scan (platform owner): run the weekly scan now.
// @Summary Scan service specs for new endpoints now
// @Tags Admin
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/endpoints/scan [post]
func (h *Handler) Scan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	added, failed := h.Scanner.ScanAll(ctx)
	if failed == nil {
		failed = []string{}
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"new_candidates": added, "failed": failed})
}

// InternalTools handles GET /api/v1/internal/endpoints/tools?tenant_id= (service key only): the
// approved routes for one tenant, tenant params already filled in, for query_service_data.
func (h *Handler) InternalTools(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "tenant_id must be a UUID", nil)
		return
	}
	t, err := h.Store.DB.Tenant.Get(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	all, err := h.Store.All(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	tools := ApprovedTools(all, h.Scanner.Bases(), tid, t.Slug)
	if tools == nil {
		tools = []Tool{}
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"tools": tools})
}

func actor(ctx context.Context) string {
	if c, ok := authclient.ClaimsFromContext(ctx); ok && c != nil {
		if c.Email != "" {
			return c.Email
		}
		return c.Subject
	}
	return "unknown"
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if ent.IsNotFound(err) {
		httpapi.Error(w, http.StatusNotFound, "not_found", "not found", nil)
		return
	}
	h.Logger.Error("endpoint discovery request failed", zap.Error(err))
	httpapi.Error(w, http.StatusInternalServerError, "internal", "request failed", nil)
}
