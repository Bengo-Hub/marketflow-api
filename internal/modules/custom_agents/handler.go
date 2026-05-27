package custom_agents

import (
	"fmt"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for custom agents.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new custom agents handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("custom_agents.handler"),
	}
}

type createAgentRequest struct {
	Name          string         `json:"name"`
	Description   *string        `json:"description"`
	TriggerType   string         `json:"trigger_type"`
	TriggerConfig map[string]any `json:"trigger_config"`
	SystemPrompt  *string        `json:"system_prompt"`
	AllowedTools  []string       `json:"allowed_tools"`
	MaxIterations int            `json:"max_iterations"`
}

type updateAgentRequest struct {
	Name          *string        `json:"name"`
	Description   *string        `json:"description"`
	TriggerType   *string        `json:"trigger_type"`
	TriggerConfig map[string]any `json:"trigger_config"`
	SystemPrompt  *string        `json:"system_prompt"`
	AllowedTools  []string       `json:"allowed_tools"`
	MaxIterations *int           `json:"max_iterations"`
	IsActive      *bool          `json:"is_active"`
}

// Create handles POST /api/v1/agents/custom
// @Summary Create a custom agent
// @Tags Custom Agents
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createAgentRequest true "Agent definition"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/custom [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	var req createAgentRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.Name == "" || req.TriggerType == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "name and trigger_type are required", nil)
		return
	}
	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:      tenantID,
		Name:          req.Name,
		Description:   req.Description,
		TriggerType:   req.TriggerType,
		TriggerConfig: req.TriggerConfig,
		SystemPrompt:  req.SystemPrompt,
		AllowedTools:  req.AllowedTools,
		MaxIterations: req.MaxIterations,
	})
	if err != nil {
		h.logger.Error("failed to create custom agent", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create custom agent", nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/agents/custom
// @Summary List custom agents
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/custom [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	results, err := h.service.List(r.Context(), tenantID)
	if err != nil {
		h.logger.Error("failed to list custom agents", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list custom agents", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": results})
}

// Get handles GET /api/v1/agents/custom/{id}
// @Summary Get a custom agent by ID
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Agent UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/custom/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid agent ID", nil)
		return
	}
	result, err := h.service.Get(r.Context(), tenantID, id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "custom agent not found", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/agents/custom/{id}
// @Summary Update a custom agent
// @Tags Custom Agents
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Agent UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/custom/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid agent ID", nil)
		return
	}
	var req updateAgentRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	result, err := h.service.Update(r.Context(), tenantID, id, UpdateInput{
		Name:          req.Name,
		Description:   req.Description,
		TriggerType:   req.TriggerType,
		TriggerConfig: req.TriggerConfig,
		SystemPrompt:  req.SystemPrompt,
		AllowedTools:  req.AllowedTools,
		MaxIterations: req.MaxIterations,
		IsActive:      req.IsActive,
	})
	if err != nil {
		h.logger.Error("failed to update custom agent", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update custom agent", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/agents/custom/{id}
// @Summary Delete a custom agent
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Agent UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /agents/custom/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid agent ID", nil)
		return
	}
	if err := h.service.Delete(r.Context(), tenantID, id); err != nil {
		h.logger.Error("failed to delete custom agent", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete custom agent", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// ListRuns handles GET /api/v1/agents/custom/{id}/runs
// @Summary Get run history for a custom agent
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Agent UUID"
// @Param limit query int false "Page size"
// @Param offset query int false "Offset"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/custom/{id}/runs [get]
func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid agent ID", nil)
		return
	}
	p := pagination.Parse(r)

	results, total, err := h.service.ListRuns(r.Context(), tenantID, id, p.Limit, p.Offset)
	if err != nil {
		h.logger.Error("failed to list agent runs", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list agent runs", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
}

// TriggerRun handles POST /api/v1/agents/custom/{id}/run
// @Summary Manually trigger a custom agent run
// @Tags Custom Agents
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Agent UUID"
// @Param request body map[string]interface{} false "Optional trigger payload"
// @Success 202 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/custom/{id}/run [post]
func (h *Handler) TriggerRun(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid agent ID", nil)
		return
	}
	var payload map[string]any
	_ = httpapi.DecodeJSON(r, &payload)

	run, err := h.service.TriggerRun(r.Context(), tenantID, id, "manual", payload)
	if err != nil {
		if err.Error() == "custom agent not found" {
			httpapi.Error(w, http.StatusNotFound, "not_found", "custom agent not found", nil)
			return
		}
		h.logger.Error("failed to trigger agent run", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "trigger_failed", "failed to trigger agent run", nil)
		return
	}
	httpapi.JSON(w, http.StatusAccepted, map[string]any{"run_id": run.ID, "status": run.Status})
}

func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

