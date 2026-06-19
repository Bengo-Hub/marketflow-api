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

// Manager resolves provider configuration using the ProviderSetting entity.
// All API keys and secrets (Groq, Meta, WhatsApp, etc.) are platform-level — NOT per-tenant.
// Tenant-specific non-secret data (FBO distrib ID, WhatsApp number, ad account ID)
// lives on the Tenant schema directly, not in ProviderSetting.
type Manager struct {
	client     *ent.Client
	keys       *encryption.KeyProvider
	env        string
	platformID string
	logger     *zap.Logger
}

// NewManager creates a new provider config manager. Decryption uses the shared
// KeyProvider (DB-first key with env fallback + legacy-plaintext passthrough).
func NewManager(client *ent.Client, keys *encryption.KeyProvider, env string, platformID string, logger *zap.Logger) *Manager {
	return &Manager{
		client:     client,
		keys:       keys,
		env:        env,
		platformID: platformID,
		logger:     logger.Named("providers"),
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

		// Decrypt if needed (tries every candidate key, falls back to legacy
		// plaintext for values stored before a key was configured).
		if r.IsEncrypted && val != "" {
			decrypted, err := m.keys.Decrypt(ctx, val)
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

// GetAILLMSettings loads AI LLM provider settings.
// AI models are platform-level shared resources — NOT per-tenant.
func (m *Manager) GetAILLMSettings(ctx context.Context) (Settings, error) {
	preferred, _ := m.getPreferred(ctx, "platform", "ai_llm")
	if preferred == "" {
		preferred = "groq"
	}
	return m.LoadSettings(ctx, "platform", "ai_llm", preferred)
}

// GetAIEmbeddingSettings loads AI embedding provider settings.
// Embeddings are platform-level shared resources — NOT per-tenant.
func (m *Manager) GetAIEmbeddingSettings(ctx context.Context) (Settings, error) {
	return m.LoadSettings(ctx, "platform", "ai_embedding", "ollama")
}

// GetMetaAdsSettings loads Meta Marketing API settings (platform-level secrets).
// The Meta App ID/Secret are platform-level. Tenant-specific data (ad account ID,
// page ID, access token) lives on the Tenant schema directly.
func (m *Manager) GetMetaAdsSettings(ctx context.Context) (Settings, error) {
	return m.LoadSettings(ctx, "platform", "meta_ads", "meta")
}

// GetSchedulingSettings loads Cal.com scheduling settings (platform-level).
func (m *Manager) GetSchedulingSettings(ctx context.Context) (Settings, error) {
	return m.LoadSettings(ctx, "platform", "scheduling", "calcom")
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
