package admin

import (
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const platformTenantID = "platform"

// PlatformProviderHandler handles platform-level provider configuration.
type PlatformProviderHandler struct {
	client        *ent.Client
	logger        *zap.Logger
	encryptionKey []byte
}

// NewPlatformProviderHandler creates a new handler.
func NewPlatformProviderHandler(client *ent.Client, encryptionKey []byte, logger *zap.Logger) *PlatformProviderHandler {
	return &PlatformProviderHandler{
		client:        client,
		logger:        logger.Named("admin.providers"),
		encryptionKey: encryptionKey,
	}
}

type configureProviderRequest struct {
	ProviderType        string            `json:"provider_type"`
	ProviderName        string            `json:"provider_name"`
	Environment         string            `json:"environment"`
	Settings            map[string]string `json:"settings"`
	PlatformManagedKeys []string          `json:"platform_managed_keys,omitempty"`
	SecretKeys          []string          `json:"secret_keys,omitempty"`
}

// ConfigureProvider creates or updates platform provider settings.
// @Summary Configure a platform provider
// @Description Upserts provider settings with optional encryption for secrets. Platform owners only.
// @Tags Admin - Providers
// @Accept json
// @Produce json
// @Param request body configureProviderRequest true "Provider configuration"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Security bearerAuth
// @Router /admin/providers/configure [post]
func (h *PlatformProviderHandler) ConfigureProvider(w http.ResponseWriter, r *http.Request) {
	var req configureProviderRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.ProviderType == "" || req.ProviderName == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "provider_type and provider_name are required", nil)
		return
	}
	if req.Environment == "" {
		req.Environment = "production"
	}

	ctx := r.Context()

	// Build set of secret keys and platform-managed keys
	secretSet := make(map[string]bool)
	for _, k := range req.SecretKeys {
		secretSet[k] = true
	}
	managedSet := make(map[string]bool)
	for _, k := range req.PlatformManagedKeys {
		managedSet[k] = true
	}

	// Delete existing settings for this provider+environment (upsert pattern)
	_, _ = h.client.ProviderSetting.Delete().
		Where(
			providersetting.TenantIDEQ(platformTenantID),
			providersetting.ProviderTypeEQ(req.ProviderType),
			providersetting.ProviderNameEQ(req.ProviderName),
			providersetting.EnvironmentEQ(req.Environment),
			providersetting.KeyNEQ("_preferred"),
		).
		Exec(ctx)

	// Create _config marker row
	_ = h.client.ProviderSetting.Create().
		SetTenantID(platformTenantID).
		SetProviderType(req.ProviderType).
		SetProviderName(req.ProviderName).
		SetEnvironment(req.Environment).
		SetKey("_config").
		SetValue("configured").
		SetIsPlatform(true).
		SetIsPlatformManaged(true).
		SetIsActive(true).
		SetStatus("active").
		Exec(ctx)

	// Create individual setting rows
	created := 0
	for key, value := range req.Settings {
		if value == "" || value == "••••••••" {
			continue // skip empty/masked values
		}

		isSecret := secretSet[key] || encryption.IsSecret(key)
		isEncrypted := false

		// Encrypt secret values
		if isSecret && len(h.encryptionKey) == 32 && value != "" {
			encrypted, err := encryption.Encrypt(value, h.encryptionKey)
			if err != nil {
				h.logger.Warn("failed to encrypt setting", zap.String("key", key), zap.Error(err))
			} else {
				value = encrypted
				isEncrypted = true
			}
		}

		err := h.client.ProviderSetting.Create().
			SetTenantID(platformTenantID).
			SetProviderType(req.ProviderType).
			SetProviderName(req.ProviderName).
			SetEnvironment(req.Environment).
			SetKey(key).
			SetValue(value).
			SetIsPlatform(true).
			SetIsPlatformManaged(managedSet[key]).
			SetIsSecret(isSecret).
			SetIsEncrypted(isEncrypted).
			SetIsActive(true).
			SetStatus("active").
			Exec(ctx)
		if err != nil {
			h.logger.Warn("failed to create setting", zap.String("key", key), zap.Error(err))
			continue
		}
		created++
	}

	// Audit trail — platform-scope, always written, masks secret values by name.
	auditKeys := make([]string, 0, len(req.Settings))
	for k := range req.Settings {
		auditKeys = append(auditKeys, k)
	}
	actor := "system"
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
		if claims.Email != "" {
			actor = claims.Email
		} else if claims.Subject != "" {
			actor = claims.Subject
		}
	}
	_, _ = h.client.AuditLog.Create().
		SetScope("platform").
		SetEvent("admin.provider_updated").
		SetActor(actor).
		SetSubjectType("provider_setting").
		SetPayload(map[string]any{
			"provider_type": req.ProviderType,
			"provider_name": req.ProviderName,
			"environment":   req.Environment,
			"keys":          auditKeys,
			"secret_keys":   req.SecretKeys,
			"settings_count": created,
		}).
		Save(r.Context())

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"status":        "configured",
		"provider_type": req.ProviderType,
		"provider_name": req.ProviderName,
		"environment":   req.Environment,
		"settings_count": created,
	})
}

// ListProviders lists all configured platform providers.
// @Summary List platform providers
// @Tags Admin - Providers
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/providers [get]
func (h *PlatformProviderHandler) ListProviders(w http.ResponseWriter, r *http.Request) {
	settings, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(platformTenantID),
			providersetting.IsPlatformEQ(true),
			providersetting.KeyEQ("_config"),
		).
		All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "failed to query providers", nil)
		return
	}

	providers := make([]map[string]any, len(settings))
	for i, s := range settings {
		providers[i] = map[string]any{
			"provider_type": s.ProviderType,
			"provider_name": s.ProviderName,
			"environment":   s.Environment,
			"is_active":     s.IsActive,
			"status":        s.Status,
		}
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{"data": providers})
}

// GetProviderSettings returns settings for a specific platform provider.
// @Summary Get platform provider settings
// @Tags Admin - Providers
// @Produce json
// @Param type query string true "Provider type (ai_llm, meta_ads, etc.)"
// @Param name query string true "Provider name (groq, ollama, meta, etc.)"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/providers/settings [get]
func (h *PlatformProviderHandler) GetProviderSettings(w http.ResponseWriter, r *http.Request) {
	providerType := r.URL.Query().Get("type")
	providerName := r.URL.Query().Get("name")
	if providerType == "" || providerName == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_params", "type and name query params required", nil)
		return
	}

	settings, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(platformTenantID),
			providersetting.ProviderTypeEQ(providerType),
			providersetting.ProviderNameEQ(providerName),
			providersetting.IsPlatformEQ(true),
			providersetting.IsActiveEQ(true),
			providersetting.KeyNEQ("_config"),
			providersetting.KeyNEQ("_preferred"),
		).
		All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "failed to query settings", nil)
		return
	}

	result := make([]map[string]any, len(settings))
	for i, s := range settings {
		value := s.Value
		if s.IsSecret || s.IsEncrypted {
			value = "••••••••"
		}
		result[i] = map[string]any{
			"key":                 s.Key,
			"value":               value,
			"is_secret":           s.IsSecret,
			"is_encrypted":        s.IsEncrypted,
			"is_platform_managed": s.IsPlatformManaged,
		}
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{"data": result})
}

// ResolveProvider returns **decrypted** provider settings for internal
// service-to-service consumers (marketflow-ai, marketflow-worker, etc.).
//
// Unlike GetProviderSettings, secrets are NOT masked — callers are expected
// to authenticate with the platform X-API-Key and use the returned values
// to bootstrap their AI/integration clients.
//
// @Summary Resolve platform provider settings (decrypted, service-only)
// @Description Returns decrypted secret values. Requires platform X-API-Key; platform_owner JWT also accepted. Not for UI display.
// @Tags Admin - Providers
// @Produce json
// @Param type query string true "Provider type (ai_llm, ai_search, meta_ads, google_calendar, etc.)"
// @Param name query string true "Provider name (groq, tavily, meta, oauth, ...)"
// @Param env query string false "Environment (default: production)"
// @Param tenant_id query string false "Tenant UUID for tenant-scoped lookup; omit for platform lookup"
// @Success 200 {object} map[string]interface{}
// @Security apiKey
// @Router /admin/providers/resolve [get]
func (h *PlatformProviderHandler) ResolveProvider(w http.ResponseWriter, r *http.Request) {
	providerType := r.URL.Query().Get("type")
	providerName := r.URL.Query().Get("name")
	env := r.URL.Query().Get("env")
	tenantID := r.URL.Query().Get("tenant_id")
	if providerType == "" || providerName == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_params", "type and name query params required", nil)
		return
	}
	if env == "" {
		env = "production"
	}
	scope := platformTenantID
	if tenantID != "" {
		scope = tenantID
	}

	rows, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(scope),
			providersetting.ProviderTypeEQ(providerType),
			providersetting.ProviderNameEQ(providerName),
			providersetting.EnvironmentEQ(env),
			providersetting.IsActiveEQ(true),
			providersetting.KeyNEQ("_config"),
			providersetting.KeyNEQ("_preferred"),
		).
		All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "failed to query settings", nil)
		return
	}

	out := make(map[string]string, len(rows))
	for _, s := range rows {
		value := s.Value
		if s.IsEncrypted && len(h.encryptionKey) > 0 {
			if dec, err := encryption.Decrypt(value, h.encryptionKey); err == nil {
				value = dec
			} else {
				h.logger.Warn("decrypt failed",
					zap.String("key", s.Key), zap.Error(err))
			}
		}
		out[s.Key] = value
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"tenant_id":     scope,
		"provider_type": providerType,
		"provider_name": providerName,
		"environment":   env,
		"settings":      out,
	})
}

// DeactivateProvider deactivates a platform provider.
// @Summary Deactivate a provider
// @Tags Admin - Providers
// @Produce json
// @Param id path string true "Provider setting ID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /admin/providers/{id} [delete]
func (h *PlatformProviderHandler) DeactivateProvider(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid provider setting ID", nil)
		return
	}

	_, err = h.client.ProviderSetting.UpdateOneID(id).
		SetIsActive(false).
		SetStatus("disabled").
		Save(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "provider setting not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]string{"status": "deactivated"})
}
