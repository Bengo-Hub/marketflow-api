package encryption

import (
	"context"
	"errors"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
)

// SettingValue returns a provider setting row's usable value. Every reader of a secret
// (OAuth tokens, client secrets) goes through here, so encrypted rows are decrypted the same
// way everywhere.
//
// Only rows flagged is_encrypted are decrypted. A legacy plaintext token can look like base64
// (Meta tokens do), so trying to decrypt every value would misread it as broken ciphertext.
//
// A legacy plaintext secret (is_secret, not yet encrypted) is re-sealed in place the first time
// it is read, when a key is configured. The update is conditional on the row still holding the
// same plaintext, so it can never overwrite a newer value. This replaces a one-off backfill: SQL
// cannot produce this AES-GCM format, and data repairs must not be scripts or endpoints.
func (p *KeyProvider) SettingValue(ctx context.Context, db *ent.Client, r *ent.ProviderSetting) (string, error) {
	if r == nil || r.Value == "" {
		return "", nil
	}
	if p == nil {
		if r.IsEncrypted {
			return "", errors.New("encrypted provider setting but no key provider wired")
		}
		return r.Value, nil
	}
	if r.IsEncrypted {
		return p.Decrypt(ctx, r.Value)
	}
	if (r.IsSecret || IsSecret(r.Key)) && db != nil && p.Configured(ctx) {
		if sealed, err := p.Encrypt(ctx, r.Value); err == nil {
			_, _ = db.ProviderSetting.Update().
				Where(
					providersetting.ID(r.ID),
					providersetting.IsEncrypted(false),
					providersetting.Value(r.Value),
				).
				SetValue(sealed).
				SetIsEncrypted(true).
				SetIsSecret(true).
				Save(ctx)
		}
	}
	return r.Value, nil
}

// SealSecret returns the value to store for a provider setting and whether it is encrypted.
// Secrets are encrypted with the primary key; with no key configured the value is stored as-is
// (the pre-existing no-key behaviour) and re-sealed later by SettingValue.
func (p *KeyProvider) SealSecret(ctx context.Context, key, value string, secret bool) (string, bool) {
	if p == nil || value == "" || !(secret || IsSecret(key)) {
		return value, false
	}
	sealed, err := p.Encrypt(ctx, value)
	if err != nil {
		return value, false
	}
	return sealed, true
}
