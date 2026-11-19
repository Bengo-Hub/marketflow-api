// Package google holds the Google OAuth calls MarketFlow makes for a YouTube channel connection:
// the code exchange at connect, the access token refresh before each upload or delete, the
// revoke on disconnect, and the channel read that names the connected account.
//
// The platform OAuth client lives in ProviderSetting tenant "platform", provider_type "youtube",
// provider_name "oauth" (client_id, client_secret encrypted at rest, redirect_url). Per channel,
// the refresh token is kept only as ciphertext on its ConnectedAccount row (refresh_ciphertext),
// with the current access token in token_ciphertext and its expiry in metadata.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
)

// Google endpoints (OAuth 2.0 for web server apps, checked 2026-11-15).
const (
	AuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	TokenURL  = "https://oauth2.googleapis.com/token"
	RevokeURL = "https://oauth2.googleapis.com/revoke"
	// YouTubeAPIBase is the Data API v3 base the channel read uses.
	YouTubeAPIBase = "https://www.googleapis.com/youtube/v3"
)

// ProviderYouTube is the ProviderSetting provider_type of the platform YouTube OAuth client.
const ProviderYouTube = "youtube"

// Client is the platform OAuth client.
type Client struct {
	ID          string
	Secret      string
	RedirectURL string
}

// LoadYouTubeClient reads the platform YouTube OAuth client from the provider settings store.
func LoadYouTubeClient(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider) (Client, error) {
	c, _, err := LoadClient(ctx, db, keys, ProviderYouTube, "oauth")
	return c, err
}

// LoadClient reads one platform Google OAuth client (Calendar, YouTube, Google Ads all use this)
// from ProviderSetting tenant "platform": client_id, client_secret and redirect_url, plus every
// other plain key in extra (Google Ads keeps developer_token, login_customer_id and api_version
// there). Secrets are decrypted (and a legacy plaintext row re-sealed) by SettingValue.
func LoadClient(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider, providerType, providerName string) (Client, map[string]string, error) {
	rows, err := db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ("platform"),
			providersetting.ProviderTypeEQ(providerType),
			providersetting.ProviderNameEQ(providerName),
			providersetting.IsActiveEQ(true),
		).
		All(ctx)
	if err != nil {
		return Client{}, nil, err
	}
	var c Client
	extra := map[string]string{}
	for _, r := range rows {
		v := r.Value
		if r.IsSecret || r.IsEncrypted {
			if v, err = keys.SettingValue(ctx, db, r); err != nil {
				return Client{}, nil, fmt.Errorf("%s oauth: decrypt %s: %w", providerType, r.Key, err)
			}
		}
		v = strings.TrimSpace(v)
		switch r.Key {
		case "client_id":
			c.ID = v
		case "client_secret":
			c.Secret = v
		case "redirect_url":
			c.RedirectURL = v
		default:
			extra[r.Key] = v
		}
	}
	if c.ID == "" || c.Secret == "" || c.RedirectURL == "" {
		return Client{}, nil, fmt.Errorf("%s oauth client not configured (client_id, client_secret and redirect_url)", providerType)
	}
	return c, extra, nil
}

// AuthCodeURL is the consent page for the scopes. access_type=offline with prompt=consent makes
// Google return a refresh token on every connect, including a reconnect.
func AuthCodeURL(c Client, scopes []string, state string) string {
	q := url.Values{
		"client_id":     {c.ID},
		"redirect_uri":  {c.RedirectURL},
		"response_type": {"code"},
		"scope":         {strings.Join(scopes, " ")},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return AuthURL + "?" + q.Encode()
}

// Token is a token endpoint answer.
type Token struct {
	Access    string
	Refresh   string // empty on a refresh: Google keeps the same refresh token
	ExpiresIn time.Duration
	Scopes    []string // granted scopes; with granular consent a person may untick some
}

// TokenError is a 4xx answer from the token endpoint.
type TokenError struct {
	Status      int
	Code        string // invalid_grant, invalid_client, ...
	Description string
}

func (e *TokenError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("google token %d %s: %s", e.Status, e.Code, e.Description)
	}
	return fmt.Sprintf("google token %d %s", e.Status, e.Code)
}

// Revoked says the refresh token no longer works (revoked, expired after six months unused, or
// a Testing-mode consent screen's 7-day limit). Only connecting again fixes it.
func (e *TokenError) Revoked() bool { return e.Code == "invalid_grant" }

// OAuth calls the token and revoke endpoints. The zero value uses Google's endpoints.
type OAuth struct {
	HTTP      *http.Client
	TokenURL  string
	RevokeURL string
}

func (o OAuth) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// Exchange trades the callback's code for tokens.
func (o OAuth) Exchange(ctx context.Context, c Client, code string) (Token, error) {
	return o.token(ctx, url.Values{
		"code":          {code},
		"client_id":     {c.ID},
		"client_secret": {c.Secret},
		"redirect_uri":  {c.RedirectURL},
		"grant_type":    {"authorization_code"},
	})
}

// Refresh gets a new access token for a stored refresh token.
func (o OAuth) Refresh(ctx context.Context, c Client, refreshToken string) (Token, error) {
	return o.token(ctx, url.Values{
		"refresh_token": {refreshToken},
		"client_id":     {c.ID},
		"client_secret": {c.Secret},
		"grant_type":    {"refresh_token"},
	})
}

func (o OAuth) token(ctx context.Context, form url.Values) (Token, error) {
	u := o.TokenURL
	if u == "" {
		u = TokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("google token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("google token: %w", err)
	}
	var out struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && out.Error != "" {
		return Token{}, &TokenError{Status: resp.StatusCode, Code: out.Error, Description: out.ErrorDescription}
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		// 5xx or an unreadable answer: worth another try later, never a reason to disconnect.
		return Token{}, fmt.Errorf("google token: status %d", resp.StatusCode)
	}
	return Token{
		Access:    out.AccessToken,
		Refresh:   out.RefreshToken,
		ExpiresIn: time.Duration(out.ExpiresIn) * time.Second,
		Scopes:    strings.Fields(out.Scope),
	}, nil
}

// Revoke ends the grant at Google, so a disconnected channel no longer lists MarketFlow under
// the person's third-party access. Revoking the refresh token revokes its access tokens too.
func (o OAuth) Revoke(ctx context.Context, token string) error {
	u := o.RevokeURL
	if u == "" {
		u = RevokeURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return fmt.Errorf("google revoke: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	// 400 invalid_token: already revoked or expired, which is the outcome we want.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		return nil
	}
	return fmt.Errorf("google revoke: status %d", resp.StatusCode)
}

// Channel is the YouTube channel a token acts for.
type Channel struct {
	ID        string
	Title     string
	CustomURL string // the @handle
	Thumbnail string
}

// MyChannels reads the channel the signed-in identity owns (channels.list mine=true, 1 quota
// unit). A Google account with brand accounts picks one channel on the consent page, so this is
// normally one channel; none means the account has no channel yet.
func MyChannels(ctx context.Context, hc *http.Client, apiBase, accessToken string) ([]Channel, error) {
	if apiBase == "" {
		apiBase = YouTubeAPIBase
	}
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	u := strings.TrimSuffix(apiBase, "/") + "/channels?part=snippet&mine=true&maxResults=50"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("youtube channels: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("youtube channels: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var ge struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &ge)
		return nil, fmt.Errorf("youtube channels: %d %s", resp.StatusCode, ge.Error.Message)
	}
	var out struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title      string `json:"title"`
				CustomURL  string `json:"customUrl"`
				Thumbnails map[string]struct {
					URL string `json:"url"`
				} `json:"thumbnails"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("youtube channels: %w", err)
	}
	chans := make([]Channel, 0, len(out.Items))
	for _, it := range out.Items {
		if it.ID == "" {
			continue
		}
		thumb := it.Snippet.Thumbnails["default"].URL
		if m := it.Snippet.Thumbnails["medium"].URL; m != "" {
			thumb = m
		}
		chans = append(chans, Channel{ID: it.ID, Title: it.Snippet.Title, CustomURL: it.Snippet.CustomURL, Thumbnail: thumb})
	}
	return chans, nil
}

// HasScopes says whether every wanted scope was granted.
func HasScopes(granted, want []string) bool {
	have := make(map[string]bool, len(granted))
	for _, s := range granted {
		have[s] = true
	}
	for _, s := range want {
		if !have[s] {
			return false
		}
	}
	return true
}
