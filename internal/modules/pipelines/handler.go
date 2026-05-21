package pipelines

import (
	"fmt"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for pipelines and stages.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new pipeline handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("pipelines.handler"),
	}
}

type createPipelineRequest struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

type updatePipelineRequest struct {
	Name      *string `json:"name"`
	IsDefault *bool   `json:"is_default"`
}

type createStageRequest struct {
	Name           string   `json:"name"`
	StageOrder     int      `json:"stage_order"`
	WinProbability *float64 `json:"win_probability"`
	IsWonStage     bool     `json:"is_won_stage"`
	IsLostStage    bool     `json:"is_lost_stage"`
	Color          *string  `json:"color"`
}

type updateStageRequest struct {
	Name           *string  `json:"name"`
	StageOrder     *int     `json:"stage_order"`
	WinProbability *float64 `json:"win_probability"`
	IsWonStage     *bool    `json:"is_won_stage"`
	IsLostStage    *bool    `json:"is_lost_stage"`
	Color          *string  `json:"color"`
}

// Create handles POST /api/v1/pipelines
// @Summary Create a pipeline
// @Tags Pipelines
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createPipelineRequest true "Pipeline data"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /pipelines [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	var req createPipelineRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.Name == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_name", "name is required", nil)
		return
	}
	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:  tenantID,
		Name:      req.Name,
		IsDefault: req.IsDefault,
	})
	if err != nil {
		h.logger.Error("failed to create pipeline", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create pipeline", nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/pipelines
// @Summary List pipelines for tenant
// @Tags Pipelines
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /pipelines [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	results, err := h.service.List(r.Context(), tenantID)
	if err != nil {
		h.logger.Error("failed to list pipelines", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list pipelines", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": results})
}

// Get handles GET /api/v1/pipelines/{id}
// @Summary Get a pipeline with its stages
// @Tags Pipelines
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Pipeline UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /pipelines/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid pipeline ID", nil)
		return
	}
	result, err := h.service.Get(r.Context(), tenantID, id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "pipeline not found", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/pipelines/{id}
// @Summary Update a pipeline
// @Tags Pipelines
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Pipeline UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /pipelines/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid pipeline ID", nil)
		return
	}
	var req updatePipelineRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	result, err := h.service.Update(r.Context(), tenantID, id, UpdateInput{
		Name:      req.Name,
		IsDefault: req.IsDefault,
	})
	if err != nil {
		h.logger.Error("failed to update pipeline", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update pipeline", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/pipelines/{id}
// @Summary Delete a pipeline
// @Tags Pipelines
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Pipeline UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /pipelines/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid pipeline ID", nil)
		return
	}
	if err := h.service.Delete(r.Context(), tenantID, id); err != nil {
		h.logger.Error("failed to delete pipeline", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete pipeline", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// CreateStage handles POST /api/v1/pipelines/{id}/stages
// @Summary Add a stage to a pipeline
// @Tags Pipelines
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Pipeline UUID"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /pipelines/{id}/stages [post]
func (h *Handler) CreateStage(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	pipelineID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid pipeline ID", nil)
		return
	}
	var req createStageRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.Name == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_name", "stage name is required", nil)
		return
	}
	result, err := h.service.CreateStage(r.Context(), StageInput{
		TenantID:       tenantID,
		PipelineID:     pipelineID,
		Name:           req.Name,
		StageOrder:     req.StageOrder,
		WinProbability: req.WinProbability,
		IsWonStage:     req.IsWonStage,
		IsLostStage:    req.IsLostStage,
		Color:          req.Color,
	})
	if err != nil {
		h.logger.Error("failed to create stage", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create stage", nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, result)
}

// UpdateStage handles PUT /api/v1/pipelines/{id}/stages/{stageId}
// @Summary Update a pipeline stage
// @Tags Pipelines
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Pipeline UUID"
// @Param stageId path string true "Stage UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /pipelines/{id}/stages/{stageId} [put]
func (h *Handler) UpdateStage(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	stageID, err := uuid.Parse(chi.URLParam(r, "stageId"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid stage ID", nil)
		return
	}
	var req updateStageRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	result, err := h.service.UpdateStage(r.Context(), tenantID, stageID, StageUpdateInput{
		Name:           req.Name,
		StageOrder:     req.StageOrder,
		WinProbability: req.WinProbability,
		IsWonStage:     req.IsWonStage,
		IsLostStage:    req.IsLostStage,
		Color:          req.Color,
	})
	if err != nil {
		h.logger.Error("failed to update stage", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update stage", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// DeleteStage handles DELETE /api/v1/pipelines/{id}/stages/{stageId}
// @Summary Delete a pipeline stage
// @Tags Pipelines
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Pipeline UUID"
// @Param stageId path string true "Stage UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /pipelines/{id}/stages/{stageId} [delete]
func (h *Handler) DeleteStage(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	stageID, err := uuid.Parse(chi.URLParam(r, "stageId"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid stage ID", nil)
		return
	}
	if err := h.service.DeleteStage(r.Context(), tenantID, stageID); err != nil {
		h.logger.Error("failed to delete stage", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete stage", nil)
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
