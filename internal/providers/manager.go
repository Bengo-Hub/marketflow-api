package providers

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"go.uber.org/zap"
)

// Settings is a key-value map of provider configuration.
type Settings map[string]string

// Manager resolves provider configuration per-tenant using the ProviderSetting entity.
// Follows the same three-level hierarchy as notifications-api:
//  1. Platform-managed settings (is_platform_managed=true, tenant_id="platform")
//  2. Tenant-specific overrides (tenant_id=<actual UUID>)
//  3. Platform fallback settings (is_platform_managed=false, tenant_id="platform")
type Manager struct {
	client        *ent.Client
	decryptionKey []byte
	env           string
	platformID    string
	logger        *zap.Logger
}

// NewManager creates a new provider config manager.
func NewManager(client *ent.Client, decryptionKey []byte, env string, platformID string, logger *zap.Logger) *Manager {
	return &Manager{
		client:        client,
		decryptionKey: decryptionKey,
		env:           env,
		platformID:    platformID,
		logger:        logger.Named("providers"),
	}
}

// LoadSettings loads and merges provider settings for a tenant with the three-level hierarchy.
func (m *Manager) LoadSettings(ctx context.Context, tenantID, providerType, providerName string) (Settings, error) {
	rows, err := m.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDIn(tenantID, "platform"),
			providersetting.ProviderTypeEQ(providerType),
			providersetting.ProviderNameEQ(providerName),
			providersetting.IsActiveEQ(true),
			providersetting.Or(
				providersetting.EnvironmentEQ(m.env),
				providersetting.EnvironmentEQ("production"),
			),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query provider settings: %w", err)
	}

	// Separate into three buckets for merge hierarchy
	platformFallback := make(Settings)
	tenantSpecific := make(Settings)
	platformManaged := make(Settings)

	for _, r := range rows {
		if r.Key == "_config" || r.Key == "_preferred" {
			continue
		}

		val := ""
		if r.Value != "" {
			val = r.Value
		}

		// Decrypt if needed
		if r.IsEncrypted && len(m.decryptionKey) == 32 && val != "" {
			decrypted, err := encryption.Decrypt(val, m.decryptionKey)
			if err != nil {
				m.logger.Warn("failed to decrypt setting",
					zap.String("key", r.Key),
					zap.String("tenant_id", r.TenantID),
					zap.Error(err))
				continue
			}
			val = decrypted
		}

		switch {
		case r.IsPlatformManaged:
			platformManaged[r.Key] = val
		case r.TenantID == tenantID && r.TenantID != "platform":
			tenantSpecific[r.Key] = val
		default:
			platformFallback[r.Key] = val
		}
	}

	// Merge: platformFallback < tenantSpecific < platformManaged
	merged := make(Settings)
	for k, v := range platformFallback {
		merged[k] = v
	}
	for k, v := range tenantSpecific {
		merged[k] = v
	}
	for k, v := range platformManaged {
		merged[k] = v
	}

	return merged, nil
}

// GetAILLMSettings loads AI LLM provider settings for a tenant.
func (m *Manager) GetAILLMSettings(ctx context.Context, tenantID string) (Settings, error) {
	// Try tenant's preferred provider first
	preferred, _ := m.getPreferred(ctx, tenantID, "ai_llm")
	if preferred == "" {
		preferred = "groq" // default
	}
	return m.LoadSettings(ctx, tenantID, "ai_llm", preferred)
}

// GetAIEmbeddingSettings loads AI embedding provider settings for a tenant.
func (m *Manager) GetAIEmbeddingSettings(ctx context.Context, tenantID string) (Settings, error) {
	return m.LoadSettings(ctx, tenantID, "ai_embedding", "ollama")
}

// GetMetaAdsSettings loads Meta Marketing API settings for a tenant.
func (m *Manager) GetMetaAdsSettings(ctx context.Context, tenantID string) (Settings, error) {
	return m.LoadSettings(ctx, tenantID, "meta_ads", "meta")
}

// GetSchedulingSettings loads Cal.com scheduling settings for a tenant.
func (m *Manager) GetSchedulingSettings(ctx context.Context, tenantID string) (Settings, error) {
	return m.LoadSettings(ctx, tenantID, "scheduling", "calcom")
}

// getPreferred returns the tenant's preferred provider for a given type.
func (m *Manager) getPreferred(ctx context.Context, tenantID, providerType string) (string, error) {
	row, err := m.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(tenantID),
			providersetting.ProviderTypeEQ(providerType),
			providersetting.KeyEQ("_preferred"),
			providersetting.IsActiveEQ(true),
		).
		First(ctx)
	if err != nil {
		return "", err
	}
	if row.Value != "" {
		return row.Value, nil
	}
	return "", nil
}

// FirstNonEmpty returns the first non-empty string from the given values.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
