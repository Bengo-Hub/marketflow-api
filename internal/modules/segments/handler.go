package segments

import (
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves segment management HTTP endpoints.
type Handler struct {
	svc    *Service
	logger *zap.Logger
}

// NewHandler creates a new segment handler.
func NewHandler(svc *Service, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, logger: logger.Named("segments.handler")}
}

func tenantID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	return id, err == nil
}

// Create handles POST /api/v1/segments.
// @Summary Create a segment
// @Tags CRM - Segments
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /segments [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	var req struct {
		Name        string         `json:"name"`
		EntityType  string         `json:"entity_type"`
		FilterRules map[string]any `json:"filter_rules"`
		IsDynamic   bool           `json:"is_dynamic"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "name is required", nil)
		return
	}
	seg, err := h.svc.Create(r.Context(), CreateInput{
		TenantID:    tid,
		Name:        req.Name,
		EntityType:  req.EntityType,
		FilterRules: req.FilterRules,
		IsDynamic:   req.IsDynamic,
	})
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, serialize(seg))
}

// List handles GET /api/v1/segments.
// @Summary List segments for the current tenant
// @Tags CRM - Segments
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /segments [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	p := pagination.Parse(r)
	segs, total, err := h.svc.List(r.Context(), tid, p.Limit, p.Offset)
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", err.Error(), nil)
		return
	}
	out := make([]map[string]any, len(segs))
	for i, s := range segs {
		out[i] = serialize(s)
	}
	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(out, total, p))
}

// Get handles GET /api/v1/segments/{id}.
// @Summary Get a segment
// @Tags CRM - Segments
// @Produce json
// @Param id path string true "Segment UUID"
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /segments/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid segment ID", nil)
		return
	}
	seg, err := h.svc.Get(r.Context(), tid, id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "segment not found", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, serialize(seg))
}

// Update handles PUT /api/v1/segments/{id}.
// @Summary Update a segment
// @Tags CRM - Segments
// @Accept json
// @Produce json
// @Param id path string true "Segment UUID"
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /segments/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid segment ID", nil)
		return
	}
	var req struct {
		Name        *string        `json:"name,omitempty"`
		FilterRules map[string]any `json:"filter_rules,omitempty"`
		IsDynamic   *bool          `json:"is_dynamic,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	seg, err := h.svc.Update(r.Context(), tid, id, UpdateInput{
		Name:        req.Name,
		FilterRules: req.FilterRules,
		IsDynamic:   req.IsDynamic,
	})
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, serialize(seg))
}

// Delete handles DELETE /api/v1/segments/{id}.
// @Summary Delete a segment
// @Tags CRM - Segments
// @Produce json
// @Param id path string true "Segment UUID"
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /segments/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid segment ID", nil)
		return
	}
	if err := h.svc.Delete(r.Context(), tid, id); err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Evaluate handles POST /api/v1/segments/{id}/evaluate.
// @Summary Re-evaluate segment rules and update member count
// @Tags CRM - Segments
// @Produce json
// @Param id path string true "Segment UUID"
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /segments/{id}/evaluate [post]
func (h *Handler) Evaluate(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid segment ID", nil)
		return
	}
	seg, err := h.svc.Evaluate(r.Context(), tid, id)
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "evaluate_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, serialize(seg))
}

func serialize(s interface{}) map[string]any {
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
