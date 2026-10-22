package worker

import (
	"context"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
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
	keys   *encryption.KeyProvider
	logger *zap.Logger
}

func NewMetaTokenResolver(db *ent.Client, keys *encryption.KeyProvider, logger *zap.Logger) *MetaTokenResolver {
	return &MetaTokenResolver{db: db, keys: keys, logger: logger.Named("meta-token-resolver")}
}

// metaPageTokenKeys are the ProviderSetting keys that hold a tenant's Page token: the OAuth
// callback writes "page_token"; "page_access_token" is the older name some rows still carry.
var metaPageTokenKeys = []string{"page_token", "page_access_token"}

// Resolve returns (token, ok) for the tenant's default Page. ok=false means no credentials for
// this tenant; callers should skip gracefully (no error logging in the hot path).
func (r *MetaTokenResolver) Resolve(ctx context.Context, tenantIDStr string) (string, bool) {
	return r.ResolvePage(ctx, tenantIDStr, "")
}

// ResolvePage returns the token of one connected Page (pageID "" means the tenant's default
// Page, tenants.meta_page_id). The ConnectedAccount row is read first; tenants that connected
// before it fall back to the ProviderSetting row, then the legacy Tenant column.
func (r *MetaTokenResolver) ResolvePage(ctx context.Context, tenantIDStr, pageID string) (string, bool) {
	if r.db == nil {
		return "", false
	}
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		return "", false
	}
	requested := pageID
	t, terr := r.db.Tenant.Query().Where(tenant.IDEQ(tenantID)).Only(ctx)
	if pageID == "" && terr == nil && t.MetaPageID != nil {
		pageID = *t.MetaPageID
	}
	if pageID != "" && r.keys != nil {
		acc, err := r.db.ConnectedAccount.Query().
			Where(
				connectedaccount.TenantID(tenantID),
				connectedaccount.AccountType("facebook_page"),
				connectedaccount.ExternalID(pageID),
				connectedaccount.Status("active"),
			).
			Only(ctx)
		if err == nil && acc.TokenCiphertext != "" {
			tok, err := r.keys.Decrypt(ctx, acc.TokenCiphertext)
			if err == nil && tok != "" {
				return tok, true
			}
			r.logger.Warn("connected page token could not be decrypted", zap.String("tenant", tenantIDStr), zap.String("page", pageID), zap.Error(err))
		}
	}
	// The older stores below hold only the default Page's token, so another Page's token can
	// come from a ConnectedAccount row alone.
	if requested != "" && (terr != nil || t.MetaPageID == nil || *t.MetaPageID != requested) {
		return "", false
	}
	// The ProviderSetting row written by the OAuth callback. Tokens are encrypted at rest, so
	// they are decrypted here (the old code skipped encrypted rows and read a key the callback
	// never wrote, so insights only worked through the legacy Tenant column).
	for _, key := range metaPageTokenKeys {
		row, err := r.db.ProviderSetting.Query().
			Where(
				providersetting.TenantIDEQ(tenantIDStr),
				providersetting.ProviderTypeEQ("meta_ads"),
				providersetting.KeyEQ(key),
				providersetting.IsActiveEQ(true),
			).
			First(ctx)
		if err != nil {
			continue
		}
		tok, err := r.keys.SettingValue(ctx, r.db, row)
		if err != nil {
			r.logger.Warn("meta page token could not be decrypted", zap.String("tenant", tenantIDStr), zap.String("key", key), zap.Error(err))
			continue
		}
		if tok != "" {
			return tok, true
		}
	}
	// Legacy fallback.
	if terr == nil && t.MetaAccessToken != nil && *t.MetaAccessToken != "" {
		return *t.MetaAccessToken, true
	}
	return "", false
}
