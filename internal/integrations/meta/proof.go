package meta

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
)

// With "Require app secret" on in the Meta app (Advanced settings), Graph
// rejects every server call made with one of the app's user or Page tokens
// unless it carries appsecret_proof, the HMAC-SHA256 of the token keyed with
// the app secret. ProofTransport adds it to every request to graph.facebook.com
// that carries a token, so no call site has to remember. Use it only for tokens
// our app issued: a tenant's own Conversions API token belongs to another app.

// Proof returns appsecret_proof for a token.
func Proof(token, appSecret string) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// SecretSource returns the app secret, or "" when none is configured (requests
// then go out unchanged, as before the setting existed).
type SecretSource func(ctx context.Context) string

type proofTransport struct {
	base   http.RoundTripper
	secret SecretSource
}

// NewProofTransport wraps base (nil means http.DefaultTransport).
func NewProofTransport(base http.RoundTripper, secret SecretSource) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &proofTransport{base: base, secret: secret}
}

func (t *proofTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Hostname(), "graph.facebook.com") || t.secret == nil {
		return t.base.RoundTrip(req)
	}
	secret := t.secret(req.Context())
	if secret == "" {
		return t.base.RoundTrip(req)
	}
	q := req.URL.Query()
	if q.Get("appsecret_proof") != "" {
		return t.base.RoundTrip(req)
	}
	// The token may be in the query, a form body or the Authorization header.
	if tok := q.Get("access_token"); tok != "" {
		r := req.Clone(req.Context())
		q.Set("appsecret_proof", Proof(tok, secret))
		r.URL.RawQuery = q.Encode()
		return t.base.RoundTrip(r)
	}
	if tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok && tok != "" {
		r := req.Clone(req.Context())
		q.Set("appsecret_proof", Proof(tok, secret))
		r.URL.RawQuery = q.Encode()
		return t.base.RoundTrip(r)
	}
	if req.Body != nil && strings.HasPrefix(req.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		raw, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		form, perr := url.ParseQuery(string(raw))
		if perr == nil && form.Get("access_token") != "" && form.Get("appsecret_proof") == "" {
			form.Set("appsecret_proof", Proof(form.Get("access_token"), secret))
			raw = []byte(form.Encode())
		}
		r := req.Clone(req.Context())
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
		return t.base.RoundTrip(r)
	}
	return t.base.RoundTrip(req)
}

// PlatformSecret reads the app secret from the platform provider row
// meta_ads/oauth (encrypted at rest), falling back to envSecret, and caches it
// for five minutes so a rotated secret takes effect without a restart.
func PlatformSecret(db *ent.Client, keys *encryption.KeyProvider, envSecret string) SecretSource {
	var (
		mu      sync.Mutex
		cached  string
		fetched time.Time
	)
	return func(ctx context.Context) string {
		mu.Lock()
		defer mu.Unlock()
		if !fetched.IsZero() && time.Since(fetched) < 5*time.Minute {
			return cached
		}
		secret := envSecret
		if db != nil && keys != nil {
			row, err := db.ProviderSetting.Query().
				Where(providersetting.TenantIDEQ("platform"), providersetting.ProviderTypeEQ("meta_ads"),
					providersetting.ProviderNameEQ("oauth"), providersetting.KeyEQ("app_secret"), providersetting.IsActiveEQ(true)).
				Only(ctx)
			if err == nil {
				if v, err := keys.SettingValue(ctx, db, row); err == nil && v != "" {
					secret = v
				}
			}
		}
		cached, fetched = secret, time.Now()
		return cached
	}
}
