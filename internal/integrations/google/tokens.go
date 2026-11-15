package google

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

// MetaAccessExpiresAt is the ConnectedAccount metadata key holding when the stored access token
// expires (RFC 3339). It is kept out of expires_at on purpose: expires_at drives the integrations
// card and the token health email, and a one-hour access token renewed on demand is not an
// expiry the business has to act on.
const MetaAccessExpiresAt = "access_expires_at"

// refreshMargin renews an access token this long before it expires, so an upload never starts
// with a token about to lapse mid chunk.
const refreshMargin = 5 * time.Minute

// ErrNoRefreshToken means the row has no refresh token, so it cannot get a new access token.
var ErrNoRefreshToken = errors.New("youtube channel has no refresh token; connect it again")

// Tokens hands out working access tokens for connected YouTube channels.
type Tokens struct {
	DB    *ent.Client
	Keys  *encryption.KeyProvider
	OAuth OAuth
	// Client loads the platform OAuth client (LoadYouTubeClient unless a test sets it).
	Client func(ctx context.Context) (Client, error)
	Now    func() time.Time
}

// NewTokens reads the platform client from the provider settings store on each refresh, so a
// rotated client secret takes effect without a restart.
func NewTokens(db *ent.Client, keys *encryption.KeyProvider) *Tokens {
	t := &Tokens{DB: db, Keys: keys, Now: time.Now}
	t.Client = func(ctx context.Context) (Client, error) { return LoadYouTubeClient(ctx, db, keys) }
	return t
}

func (t *Tokens) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
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

// Refresh asks Google for a new access token and stores it. When Google answers invalid_grant
// the row is marked expired, so the integrations card asks for a reconnect and the publish
// dialog stops offering the channel; the returned *TokenError says so to the caller. Any other
// failure leaves the row as it is.
func (t *Tokens) Refresh(ctx context.Context, acc *ent.ConnectedAccount) (string, error) {
	if acc.RefreshCiphertext == "" {
		return "", ErrNoRefreshToken
	}
	refresh, err := t.Keys.Decrypt(ctx, acc.RefreshCiphertext)
	if err != nil || refresh == "" {
		return "", fmt.Errorf("youtube refresh token unreadable: %w", err)
	}
	c, err := t.Client(ctx)
	if err != nil {
		return "", err
	}
	tok, err := t.OAuth.Refresh(ctx, c, refresh)
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
		// The token works; it just cannot be cached, so the next call refreshes again.
		return tok.Access, nil
	}
	patch, _ := json.Marshal(map[string]string{
		MetaAccessExpiresAt: t.now().Add(tok.ExpiresIn).UTC().Format(time.RFC3339),
	})
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
	// A failed save only costs a refresh on the next call; the caller still gets a working token.
	_ = upd.Exec(ctx)
	return tok.Access, nil
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
