package tasks

import (
	"fmt"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for tasks.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new task handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("tasks.handler"),
	}
}

type createTaskRequest struct {
	EntityType       string     `json:"entity_type"`
	EntityID         uuid.UUID  `json:"entity_id"`
	Title            string     `json:"title"`
	Description      *string    `json:"description"`
	DueAt            *string    `json:"due_at"`
	Priority         string     `json:"priority"`
	AssignedToUserID *uuid.UUID `json:"assigned_to_user_id"`
}

type updateTaskRequest struct {
	Title            *string    `json:"title"`
	Description      *string    `json:"description"`
	DueAt            *string    `json:"due_at"`
	Priority         *string    `json:"priority"`
	Status           *string    `json:"status"`
	AssignedToUserID *uuid.UUID `json:"assigned_to_user_id"`
}

// Create handles POST /api/v1/tasks
// @Summary Create a task
// @Tags Tasks
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createTaskRequest true "Task data"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tasks [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	var req createTaskRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.Title == "" || req.EntityType == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "title and entity_type are required", nil)
		return
	}

	in := CreateInput{
		TenantID:         tenantID,
		EntityType:       req.EntityType,
		EntityID:         req.EntityID,
		Title:            req.Title,
		Description:      req.Description,
		Priority:         req.Priority,
		AssignedToUserID: req.AssignedToUserID,
	}
	if req.DueAt != nil {
		if t, err := time.Parse(time.RFC3339, *req.DueAt); err == nil {
			in.DueAt = &t
		} else if t, err := time.Parse("2006-01-02", *req.DueAt); err == nil {
			in.DueAt = &t
		}
	}

	result, err := h.service.Create(r.Context(), in)
	if err != nil {
		h.logger.Error("failed to create task", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create task", nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/tasks
// @Summary List tasks
// @Tags Tasks
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param entity_type query string false "lead | contact | deal"
// @Param entity_id query string false "Entity UUID"
// @Param status query string false "open | in_progress | done"
// @Param overdue query bool false "Only overdue tasks"
// @Param limit query int false "Page size"
// @Param offset query int false "Offset"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tasks [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	in := ListInput{
		TenantID: tenantID,
		Limit:    parseIntQuery(r, "limit", 20),
		Offset:   parseIntQuery(r, "offset", 0),
		Overdue:  r.URL.Query().Get("overdue") == "true",
	}
	if s := r.URL.Query().Get("entity_type"); s != "" {
		in.EntityType = &s
	}
	if s := r.URL.Query().Get("entity_id"); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			in.EntityID = &id
		}
	}
	if s := r.URL.Query().Get("status"); s != "" {
		in.Status = &s
	}
	if s := r.URL.Query().Get("assigned_to"); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			in.AssignedTo = &id
		}
	}

	results, total, err := h.service.List(r.Context(), in)
	if err != nil {
		h.logger.Error("failed to list tasks", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list tasks", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": results, "total": total})
}

// Get handles GET /api/v1/tasks/{id}
// @Summary Get a task by ID
// @Tags Tasks
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Task UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tasks/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid task ID", nil)
		return
	}
	result, err := h.service.Get(r.Context(), tenantID, id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "task not found", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/tasks/{id}
// @Summary Update a task
// @Tags Tasks
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Task UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tasks/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid task ID", nil)
		return
	}
	var req updateTaskRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	in := UpdateInput{
		Title:            req.Title,
		Description:      req.Description,
		Priority:         req.Priority,
		Status:           req.Status,
		AssignedToUserID: req.AssignedToUserID,
	}
	if req.DueAt != nil {
		if t, err := time.Parse(time.RFC3339, *req.DueAt); err == nil {
			in.DueAt = &t
		} else if t, err := time.Parse("2006-01-02", *req.DueAt); err == nil {
			in.DueAt = &t
		}
	}

	result, err := h.service.Update(r.Context(), tenantID, id, in)
	if err != nil {
		h.logger.Error("failed to update task", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update task", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Complete handles POST /api/v1/tasks/{id}/complete
// @Summary Mark a task as done
// @Tags Tasks
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Task UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tasks/{id}/complete [post]
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid task ID", nil)
		return
	}
	result, err := h.service.Complete(r.Context(), tenantID, id)
	if err != nil {
		h.logger.Error("failed to complete task", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "complete_failed", "failed to complete task", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/tasks/{id}
// @Summary Delete a task
// @Tags Tasks
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Task UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /tasks/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid task ID", nil)
		return
	}
	if err := h.service.Delete(r.Context(), tenantID, id); err != nil {
		h.logger.Error("failed to delete task", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete task", nil)
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
