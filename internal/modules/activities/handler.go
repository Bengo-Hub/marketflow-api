package activities

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for activities.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new activity handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("activities.handler"),
	}
}

type logActivityRequest struct {
	EntityType      string     `json:"entity_type"`
	EntityID        uuid.UUID  `json:"entity_id"`
	ActivityType    string     `json:"activity_type"`
	Direction       *string    `json:"direction"`
	Subject         *string    `json:"subject"`
	Body            *string    `json:"body"`
	Outcome         *string    `json:"outcome"`
	DurationMinutes *int       `json:"duration_minutes"`
	PerformedBy     *uuid.UUID `json:"performed_by"`
	Source          *string    `json:"source"`
	ExternalRef     *string    `json:"external_ref"`
	PerformedAt     *string    `json:"performed_at"`
}

// Log handles POST /api/v1/activities
// @Summary Log an activity for any CRM entity
// @Tags Activities
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body logActivityRequest true "Activity data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /activities [post]
func (h *Handler) Log(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	var req logActivityRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.EntityType == "" || req.ActivityType == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "entity_type and activity_type are required", nil)
		return
	}

	in := CreateInput{
		TenantID:        tenantID,
		EntityType:      req.EntityType,
		EntityID:        req.EntityID,
		ActivityType:    req.ActivityType,
		Direction:       req.Direction,
		Subject:         req.Subject,
		Body:            req.Body,
		Outcome:         req.Outcome,
		DurationMinutes: req.DurationMinutes,
		PerformedBy:     req.PerformedBy,
		Source:          req.Source,
		ExternalRef:     req.ExternalRef,
	}
	if req.PerformedAt != nil {
		if t, err := time.Parse(time.RFC3339, *req.PerformedAt); err == nil {
			in.PerformedAt = &t
		}
	}

	result, err := h.service.Log(r.Context(), in)
	if err != nil {
		h.logger.Error("failed to log activity", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "log_failed", "failed to log activity", nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, result)
}

// ListForEntity handles GET /api/v1/{entityType}/{entityId}/timeline
// entity_type is extracted from the URL path e.g. leads/{id}/timeline, contacts/{id}/timeline, deals/{id}/timeline
// This single handler is registered for each entity path.
func (h *Handler) ListForEntity(entityType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID, err := extractTenantID(r)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
			return
		}
		entityID, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid entity ID", nil)
			return
		}

		p := pagination.Parse(r)
		in := ListInput{
			TenantID:   tenantID,
			EntityType: entityType,
			EntityID:   entityID,
			Limit:      p.Limit,
			Offset:     p.Offset,
		}
		if at := r.URL.Query().Get("activity_type"); at != "" {
			in.ActivityType = &at
		}

		results, total, err := h.service.List(r.Context(), in)
		if err != nil {
			h.logger.Error("failed to list timeline", zap.Error(err))
			httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list activities", nil)
			return
		}
		httpapi.JSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
	}
}

// GetTimeline handles GET /api/v1/activities — general list by entity_type + entity_id query params
// @Summary List activities for an entity
// @Tags Activities
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param entity_type query string false "lead | contact | deal (omit for the whole tenant)"
// @Param entity_id query string false "Entity UUID"
// @Param activity_type query string false "Filter by type"
// @Param limit query int false "Page size"
// @Param offset query int false "Offset"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /activities [get]
func (h *Handler) GetTimeline(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	// Both filters are optional: without them this is the tenant-wide activity feed.
	entityType := r.URL.Query().Get("entity_type")
	if entityType != "" && entityType != "lead" && entityType != "contact" && entityType != "deal" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_entity_type", "entity_type must be lead, contact or deal", nil)
		return
	}
	var entityID uuid.UUID
	if s := r.URL.Query().Get("entity_id"); s != "" {
		if entityID, err = uuid.Parse(s); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_entity_id", "invalid entity_id", nil)
			return
		}
	}

	p := pagination.Parse(r)
	in := ListInput{
		TenantID:   tenantID,
		EntityType: entityType,
		EntityID:   entityID,
		Limit:      p.Limit,
		Offset:     p.Offset,
	}
	if at := r.URL.Query().Get("activity_type"); at != "" {
		in.ActivityType = &at
	}

	results, total, err := h.service.List(r.Context(), in)
	if err != nil {
		h.logger.Error("failed to list activities", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list activities", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
}

// ListSuggestions handles GET /api/v1/suggestions
// @Summary List unread proactive agent suggestions
// @Tags Suggestions
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param limit query int false "Max results (default 20)"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /suggestions [get]
func (h *Handler) ListSuggestions(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	p := pagination.Parse(r)
	results, err := h.service.ListSuggestions(r.Context(), tenantID, p.Limit)
	if err != nil {
		h.logger.Error("failed to list suggestions", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list suggestions", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": results, "count": len(results)})
}

// DismissSuggestion handles POST /api/v1/suggestions/{id}/dismiss
// @Summary Dismiss a proactive suggestion
// @Tags Suggestions
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Activity UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /suggestions/{id}/dismiss [post]
func (h *Handler) DismissSuggestion(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid suggestion ID", nil)
		return
	}
	if err := h.service.DismissSuggestion(r.Context(), tenantID, id); err != nil {
		if err.Error() == "suggestion not found" {
			httpapi.Error(w, http.StatusNotFound, "not_found", "suggestion not found", nil)
			return
		}
		h.logger.Error("failed to dismiss suggestion", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "dismiss_failed", "failed to dismiss suggestion", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "dismissed"})
}

func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

