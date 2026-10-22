package gcal

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
)

// loadPlatformCreds reads the shared Google OAuth client config from the
// platform-level ProviderSetting rows. client_secret is encrypted at rest.
func loadPlatformCreds(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider) (PlatformCreds, error) {
	rows, err := db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ("platform"),
			providersetting.ProviderTypeEQ("google_calendar"),
			providersetting.ProviderNameEQ("oauth"),
		).
		All(ctx)
	if err != nil {
		return PlatformCreds{}, err
	}
	out := PlatformCreds{}
	for _, r := range rows {
		switch r.Key {
		case "client_id":
			out.ClientID = r.Value
		case "client_secret":
			v, err := keys.SettingValue(ctx, db, r)
			if err != nil {
				return PlatformCreds{}, fmt.Errorf("gcal: decrypt client_secret: %w", err)
			}
			out.ClientSecret = v
		case "redirect_url":
			out.RedirectURL = r.Value
		}
	}
	if out.ClientID == "" || out.ClientSecret == "" {
		return PlatformCreds{}, fmt.Errorf("gcal: platform oauth not configured")
	}
	return out, nil
}

// loadTenantCreds reads a tenant's linked refresh_token + chosen calendar_id
// from their ProviderSetting rows. The refresh_token is encrypted at rest; keys
// decrypts it (and re-seals a legacy plaintext row).
func loadTenantCreds(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider, tenantID string) (TenantCreds, error) {
	rows, err := db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(tenantID),
			providersetting.ProviderTypeEQ("google_calendar"),
			providersetting.ProviderNameEQ("oauth"),
		).
		All(ctx)
	if err != nil {
		return TenantCreds{}, err
	}
	out := TenantCreds{}
	for _, r := range rows {
		switch r.Key {
		case "refresh_token":
			v, err := keys.SettingValue(ctx, db, r)
			if err != nil {
				return TenantCreds{}, fmt.Errorf("gcal: decrypt refresh_token: %w", err)
			}
			out.RefreshToken = v
		case "calendar_id":
			out.CalendarID = r.Value
		}
	}
	if out.RefreshToken == "" {
		return TenantCreds{}, fmt.Errorf("gcal: tenant has no refresh_token")
	}
	return out, nil
}
