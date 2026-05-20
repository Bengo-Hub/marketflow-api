package admin

import (
	"encoding/json"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ServiceConfigHandler handles service-level configuration CRUD for marketflow-api.
type ServiceConfigHandler struct {
	client *ent.Client
	logger *zap.Logger
}

// NewServiceConfigHandler creates a new ServiceConfigHandler.
func NewServiceConfigHandler(client *ent.Client, logger *zap.Logger) *ServiceConfigHandler {
	return &ServiceConfigHandler{
		client: client,
		logger: logger.Named("admin.service-config"),
	}
}

type mfSCResponse struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    *uuid.UUID `json:"tenant_id,omitempty"`
	ConfigKey   string     `json:"config_key"`
	ConfigValue string     `json:"config_value"`
	ConfigType  string     `json:"config_type"`
	Description string     `json:"description,omitempty"`
	IsSecret    bool       `json:"is_secret"`
	IsOverride  bool       `json:"is_override"`
	CreatedAt   string     `json:"created_at"`
	UpdatedAt   string     `json:"updated_at"`
}

func toMFSCResponse(cfg *ent.ServiceConfig, isOverride bool) mfSCResponse {
	val := cfg.ConfigValue
	if cfg.IsSecret {
		val = "***"
	}
	return mfSCResponse{
		ID:          cfg.ID,
		TenantID:    cfg.TenantID,
		ConfigKey:   cfg.ConfigKey,
		ConfigValue: val,
		ConfigType:  cfg.ConfigType,
		Description: cfg.Description,
		IsSecret:    cfg.IsSecret,
		IsOverride:  isOverride,
		CreatedAt:   cfg.CreatedAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   cfg.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

// ListPlatformSettings returns all platform-level (tenant_id=nil) service configs.
// GET /api/v1/admin/service-config
func (h *ServiceConfigHandler) ListPlatformSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	configs, err := h.client.ServiceConfig.Query().
		Where(serviceconfig.TenantIDIsNil()).
		All(ctx)
	if err != nil {
		h.logger.Error("failed to list platform configs", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "failed to list platform settings", nil)
		return
	}

	result := make([]mfSCResponse, 0, len(configs))
	for _, cfg := range configs {
		result = append(result, mfSCResponse{
			ID:          cfg.ID,
			TenantID:    cfg.TenantID,
			ConfigKey:   cfg.ConfigKey,
			ConfigValue: cfg.ConfigValue, // unmasked for platform admin
			ConfigType:  cfg.ConfigType,
			Description: cfg.Description,
			IsSecret:    cfg.IsSecret,
			IsOverride:  false,
			CreatedAt:   cfg.CreatedAt.Format("2006-01-02T15:04:05Z"),
			UpdatedAt:   cfg.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{"data": result, "total": len(result)})
}

// UpsertPlatformSetting updates a platform-level config by key.
// PUT /api/v1/admin/service-config/{key}
func (h *ServiceConfigHandler) UpsertPlatformSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_key", "config key is required", nil)
		return
	}

	var req struct {
		ConfigValue string `json:"config_value"`
		Description string `json:"description,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.ConfigValue == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_value", "config_value is required", nil)
		return
	}

	ctx := r.Context()

	existing, err := h.client.ServiceConfig.Query().
		Where(
			serviceconfig.ConfigKey(key),
			serviceconfig.TenantIDIsNil(),
		).
		First(ctx)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "platform default not found for key: "+key, nil)
		return
	}

	update := existing.Update().SetConfigValue(req.ConfigValue)
	if req.Description != "" {
		update = update.SetDescription(req.Description)
	}

	cfg, err := update.Save(ctx)
	if err != nil {
		h.logger.Error("failed to update platform config", zap.Error(err), zap.String("key", key))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update platform setting", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, toMFSCResponse(cfg, false))
}

// ListTenantSettings returns merged tenant+platform service configs.
// GET /api/v1/admin/service-config/tenant/{tenantId}
func (h *ServiceConfigHandler) ListTenantSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tenantIDStr := chi.URLParam(r, "tenantId")
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "invalid tenant ID", nil)
		return
	}

	platformConfigs, err := h.client.ServiceConfig.Query().
		Where(serviceconfig.TenantIDIsNil()).
		All(ctx)
	if err != nil {
		h.logger.Error("failed to list platform configs", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "failed to list settings", nil)
		return
	}

	tenantConfigs, err := h.client.ServiceConfig.Query().
		Where(serviceconfig.TenantID(tenantID)).
		All(ctx)
	if err != nil {
		h.logger.Error("failed to list tenant configs", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "failed to list settings", nil)
		return
	}

	tenantByKey := make(map[string]*ent.ServiceConfig, len(tenantConfigs))
	for _, tc := range tenantConfigs {
		tenantByKey[tc.ConfigKey] = tc
	}

	result := make([]mfSCResponse, 0, len(platformConfigs))
	for _, pc := range platformConfigs {
		if tc, ok := tenantByKey[pc.ConfigKey]; ok {
			result = append(result, toMFSCResponse(tc, true))
			delete(tenantByKey, pc.ConfigKey)
		} else {
			result = append(result, toMFSCResponse(pc, false))
		}
	}
	for _, tc := range tenantByKey {
		result = append(result, toMFSCResponse(tc, true))
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{"data": result, "total": len(result)})
}

// UpsertTenantSetting creates or updates a tenant-specific config override.
// PUT /api/v1/admin/service-config/tenant/{tenantId}/{key}
func (h *ServiceConfigHandler) UpsertTenantSetting(w http.ResponseWriter, r *http.Request) {
	tenantIDStr := chi.URLParam(r, "tenantId")
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "invalid tenant ID", nil)
		return
	}

	key := chi.URLParam(r, "key")
	if key == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_key", "config key is required", nil)
		return
	}

	var req struct {
		ConfigValue string `json:"config_value"`
		Description string `json:"description,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.ConfigValue == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_value", "config_value is required", nil)
		return
	}

	ctx := r.Context()

	platformDefault, _ := h.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKey(key), serviceconfig.TenantIDIsNil()).
		First(ctx)

	configType := "string"
	if platformDefault != nil {
		configType = platformDefault.ConfigType
	}

	existing, _ := h.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKey(key), serviceconfig.TenantID(tenantID)).
		First(ctx)

	var cfg *ent.ServiceConfig
	if existing != nil {
		cfg, err = existing.Update().SetConfigValue(req.ConfigValue).Save(ctx)
	} else {
		create := h.client.ServiceConfig.Create().
			SetTenantID(tenantID).
			SetConfigKey(key).
			SetConfigValue(req.ConfigValue).
			SetConfigType(configType)
		if req.Description != "" {
			create = create.SetDescription(req.Description)
		} else if platformDefault != nil && platformDefault.Description != "" {
			create = create.SetDescription(platformDefault.Description)
		}
		if platformDefault != nil {
			create = create.SetIsSecret(platformDefault.IsSecret)
		}
		cfg, err = create.Save(ctx)
	}

	if err != nil {
		h.logger.Error("failed to upsert tenant config", zap.Error(err), zap.String("key", key))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to save setting", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, toMFSCResponse(cfg, true))
}
