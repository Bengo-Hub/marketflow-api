package custom_fields

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for custom fields.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new custom fields handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("custom_fields.handler"),
	}
}

type createDefRequest struct {
	EntityType string   `json:"entity_type"`
	FieldKey   string   `json:"field_key"`
	FieldLabel string   `json:"field_label"`
	FieldType  string   `json:"field_type"`
	Options    []string `json:"options"`
	IsRequired bool     `json:"is_required"`
	SortOrder  int      `json:"sort_order"`
}

type updateDefRequest struct {
	FieldLabel *string  `json:"field_label"`
	Options    []string `json:"options"`
	IsRequired *bool    `json:"is_required"`
	SortOrder  *int     `json:"sort_order"`
}

type setValuesRequest struct {
	Values []struct {
		FieldDefID uuid.UUID `json:"field_def_id"`
		Value      string    `json:"value"`
	} `json:"values"`
}

// CreateDef handles POST /api/v1/custom-fields/defs
// @Summary Create a custom field definition
// @Tags Custom Fields
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createDefRequest true "Field definition"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /custom-fields/defs [post]
func (h *Handler) CreateDef(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	var req createDefRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.EntityType == "" || req.FieldKey == "" || req.FieldLabel == "" || req.FieldType == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "entity_type, field_key, field_label, and field_type are required", nil)
		return
	}
	result, err := h.service.CreateDef(r.Context(), CreateDefInput{
		TenantID:   tenantID,
		EntityType: req.EntityType,
		FieldKey:   req.FieldKey,
		FieldLabel: req.FieldLabel,
		FieldType:  req.FieldType,
		Options:    req.Options,
		IsRequired: req.IsRequired,
		SortOrder:  req.SortOrder,
	})
	if err != nil {
		h.writeErr(w, err, "create_failed", "failed to create custom field definition")
		return
	}
	audit.Tenant(r.Context(), h.service.client, h.logger, tenantID, "custom_field.created", "custom_field", &result.ID,
		map[string]any{"entity_type": result.EntityType, "field_key": result.FieldKey})
	httpapi.JSON(w, http.StatusCreated, result)
}

// ListDefs handles GET /api/v1/custom-fields/defs
// @Summary List custom field definitions
// @Tags Custom Fields
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param entity_type query string false "Filter by entity type"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /custom-fields/defs [get]
func (h *Handler) ListDefs(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	var entityType *string
	if et := r.URL.Query().Get("entity_type"); et != "" {
		entityType = &et
	}
	results, err := h.service.ListDefs(r.Context(), tenantID, entityType)
	if err != nil {
		h.logger.Error("failed to list custom field defs", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list custom field definitions", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": results})
}

// UpdateDef handles PUT /api/v1/custom-fields/defs/{id}
// @Summary Update a custom field definition
// @Tags Custom Fields
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Definition UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /custom-fields/defs/{id} [put]
func (h *Handler) UpdateDef(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid definition ID", nil)
		return
	}
	var req updateDefRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	result, err := h.service.UpdateDef(r.Context(), tenantID, id, UpdateDefInput{
		FieldLabel: req.FieldLabel,
		Options:    req.Options,
		IsRequired: req.IsRequired,
		SortOrder:  req.SortOrder,
	})
	if err != nil {
		h.writeErr(w, err, "update_failed", "failed to update custom field definition")
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// DeleteDef handles DELETE /api/v1/custom-fields/defs/{id}
// @Summary Delete a custom field definition
// @Tags Custom Fields
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Definition UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /custom-fields/defs/{id} [delete]
func (h *Handler) DeleteDef(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid definition ID", nil)
		return
	}
	removed, err := h.service.DeleteDef(r.Context(), tenantID, id)
	if err != nil {
		h.writeErr(w, err, "delete_failed", "failed to delete custom field definition")
		return
	}
	audit.Tenant(r.Context(), h.service.client, h.logger, tenantID, "custom_field.deleted", "custom_field", &id, map[string]any{"values_removed": removed})
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted", "values_removed": removed})
}

// SetValues handles PUT /api/v1/{entityType}/{entityId}/custom-fields
// This is a single handler registered for leads, contacts, and deals.
func (h *Handler) SetValues(entityType string) http.HandlerFunc {
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
		var req setValuesRequest
		if err := httpapi.DecodeJSON(r, &req); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
			return
		}

		results := make([]*ent.CustomFieldValue, 0, len(req.Values))
		for _, v := range req.Values {
			val, err := h.service.SetValue(r.Context(), SetValueInput{
				TenantID:   tenantID,
				EntityType: entityType,
				EntityID:   entityID,
				FieldDefID: v.FieldDefID,
				Value:      v.Value,
			})
			if err != nil {
				h.logger.Error("failed to set custom field value", zap.Error(err))
				httpapi.Error(w, http.StatusInternalServerError, "set_failed", "failed to set custom field value", nil)
				return
			}
			results = append(results, val)
		}
		httpapi.JSON(w, http.StatusOK, map[string]any{"data": results})
	}
}

// GetValues handles GET /api/v1/{entityType}/{entityId}/custom-fields
func (h *Handler) GetValues(entityType string) http.HandlerFunc {
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
		results, err := h.service.GetValues(r.Context(), tenantID, entityType, entityID)
		if err != nil {
			h.logger.Error("failed to get custom field values", zap.Error(err))
			httpapi.Error(w, http.StatusInternalServerError, "get_failed", "failed to get custom field values", nil)
			return
		}
		httpapi.JSON(w, http.StatusOK, map[string]any{"data": results})
	}
}

// writeErr answers a definition error: 400 for bad input, 409 for a taken key, 404 for an
// unknown id, and a generic 500 (logged, never echoed) for anything else.
func (h *Handler) writeErr(w http.ResponseWriter, err error, code, msg string) {
	switch {
	case errors.Is(err, ErrInvalid):
		httpapi.Error(w, http.StatusBadRequest, "invalid_field", err.Error(), nil)
	case errors.Is(err, ErrDuplicate):
		httpapi.Error(w, http.StatusConflict, "duplicate_field", err.Error(), nil)
	case errors.Is(err, ErrNotFound):
		httpapi.Error(w, http.StatusNotFound, "not_found", "custom field not found", nil)
	default:
		h.logger.Error(msg, zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, code, msg, nil)
	}
}

func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}
