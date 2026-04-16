package worker

import (
	"context"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MetaTokenResolver returns a tenant's Meta page access token by reading
// the per-tenant ProviderSetting row written by the /integrations/meta/
// OAuth callback, with a fallback to the legacy Tenant.meta_access_token
// column for tenants that connected before the ProviderSetting refactor.
type MetaTokenResolver struct {
	db     *ent.Client
	logger *zap.Logger
}

func NewMetaTokenResolver(db *ent.Client, logger *zap.Logger) *MetaTokenResolver {
	return &MetaTokenResolver{db: db, logger: logger.Named("meta-token-resolver")}
}

// Resolve returns (token, ok). ok=false means no credentials for this tenant;
// callers should skip gracefully (no error logging in the hot path).
func (r *MetaTokenResolver) Resolve(ctx context.Context, tenantIDStr string) (string, bool) {
	if r.db == nil {
		return "", false
	}
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		return "", false
	}
	// Prefer the ProviderSetting row — it stores decrypted lookups via the
	// admin resolve endpoint, but within the same process we can query Ent
	// directly and let the secrets stay encrypted (we don't decrypt the
	// access token for the insights read; it flows as-is to the caller which
	// passes through to Meta — so we only fetch plaintext cells).
	rows, err := r.db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(tenantIDStr),
			providersetting.ProviderTypeEQ("meta_ads"),
			providersetting.KeyEQ("page_access_token"),
			providersetting.IsActiveEQ(true),
		).
		Limit(1).
		All(ctx)
	if err == nil && len(rows) > 0 && rows[0].Value != "" && !rows[0].IsEncrypted {
		return rows[0].Value, true
	}
	// Legacy fallback.
	t, err := r.db.Tenant.Query().Where(tenant.IDEQ(tenantID)).Only(ctx)
	if err == nil && t.MetaAccessToken != nil && *t.MetaAccessToken != "" {
		return *t.MetaAccessToken, true
	}
	return "", false
}
