package tiktok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
)

// refreshMargin renews an access token this long before it expires, so an upload never starts
// with a token about to lapse mid chunk.
const refreshMargin = 10 * time.Minute

// ErrNoRefreshToken means the row has no refresh token, so it cannot get a new access token.
var ErrNoRefreshToken = errors.New("the connected TikTok account has no refresh token; connect it again")

// Tokens hands out working access tokens for connected TikTok accounts, the same way
// google.Tokens does for YouTube channels.
type Tokens struct {
	DB    *ent.Client
	Keys  *encryption.KeyProvider
	OAuth OAuth
	// App loads the platform app (LoadApp unless a test sets it); read on each refresh so a
	// rotated client secret takes effect without a restart.
	App func(ctx context.Context) (App, error)
	Now func() time.Time
}

// NewTokens reads the platform app from the provider settings store.
func NewTokens(db *ent.Client, keys *encryption.KeyProvider) *Tokens {
	return &Tokens{DB: db, Keys: keys, Now: time.Now,
		App: func(ctx context.Context) (App, error) { return LoadApp(ctx, db, keys) }}
}

func (t *Tokens) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// AccessExpiry is when the stored access token expires (zero when unknown).
func AccessExpiry(acc *ent.ConnectedAccount) time.Time {
	if v, ok := acc.Metadata[MetaAccessExpiresAt].(string); ok {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			return ts
		}
	}
	return time.Time{}
}

// AccessToken returns the stored access token while it has more than refreshMargin left, and a
// refreshed one otherwise.
func (t *Tokens) AccessToken(ctx context.Context, acc *ent.ConnectedAccount) (string, error) {
	if acc.TokenCiphertext != "" && AccessExpiry(acc).After(t.now().Add(refreshMargin)) {
		if tok, err := t.Keys.Decrypt(ctx, acc.TokenCiphertext); err == nil && tok != "" {
			return tok, nil
		}
	}
	return t.Refresh(ctx, acc)
}

// Refresh asks TikTok for a new access token and stores it with the (possibly rotated) refresh
// token. When TikTok answers invalid_grant the row is marked expired, so the integrations card
// asks for a reconnect and the publish dialog stops offering the account.
func (t *Tokens) Refresh(ctx context.Context, acc *ent.ConnectedAccount) (string, error) {
	if acc.RefreshCiphertext == "" {
		return "", ErrNoRefreshToken
	}
	refresh, err := t.Keys.Decrypt(ctx, acc.RefreshCiphertext)
	if err != nil || refresh == "" {
		return "", fmt.Errorf("tiktok refresh token unreadable: %w", err)
	}
	app, err := t.App(ctx)
	if err != nil {
		return "", err
	}
	tok, err := t.OAuth.Refresh(ctx, app, refresh)
	if err != nil {
		var te *TokenError
		if errors.As(err, &te) && te.Revoked() {
			if uerr := t.DB.ConnectedAccount.UpdateOneID(acc.ID).
				Where(connectedaccount.StatusEQ("active")).
				SetStatus("expired").Exec(ctx); uerr != nil && !ent.IsNotFound(uerr) {
				return "", errors.Join(err, uerr)
			}
		}
		return "", err
	}
	sealed, err := t.Keys.Encrypt(ctx, tok.Access)
	if err != nil {
		return tok.Access, nil // works now; the next call refreshes again
	}
	now := t.now()
	md := map[string]string{MetaAccessExpiresAt: now.Add(tok.ExpiresIn).UTC().Format(time.RFC3339)}
	if tok.RefreshExpiresIn > 0 {
		md[MetaRefreshExpiresAt] = now.Add(tok.RefreshExpiresIn).UTC().Format(time.RFC3339)
	}
	patch, _ := json.Marshal(md)
	value := entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("(COALESCE(" + connectedaccount.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(patch))
		b.WriteString("::jsonb)")
	})
	upd := t.DB.ConnectedAccount.UpdateOneID(acc.ID).
		SetTokenCiphertext(sealed).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(connectedaccount.FieldMetadata, value) })
	if tok.Refresh != "" && tok.Refresh != refresh {
		if sealedRefresh, err := t.Keys.Encrypt(ctx, tok.Refresh); err == nil {
			upd = upd.SetRefreshCiphertext(sealedRefresh)
		}
	}
	if len(tok.Scopes) > 0 {
		upd = upd.SetScopes(tok.Scopes)
	}
	// A failed save only costs a refresh on the next call; the caller still gets a working token.
	_ = upd.Exec(ctx)
	return tok.Access, nil
}
