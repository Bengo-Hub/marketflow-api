package tiktok

// TikTok for Developers (Login Kit for Web plus the Content Posting API): a business connects its
// own TikTok account so MarketFlow can post the videos it approves. This is a different app and
// API from the TikTok for Business client in client.go (ads and leads).
//
// The platform app is ProviderSetting tenant "platform", provider_type "tiktok", provider_name
// "oauth": client_key, client_secret (sealed) and redirect_url. Each creator account is a
// ConnectedAccount (platform "tiktok", account_type "tiktok_creator", external_id = open_id)
// holding the access and refresh tokens as ciphertext. Access tokens last 24 hours and are
// renewed with the refresh token (365 days) before use (Tokens).

import (
	"context"
	"encoding/json"
	"errors"
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

const (
	// AuthorizeURL is the Login Kit consent page. TikTok calls the app id client_key.
	AuthorizeURL = "https://www.tiktok.com/v2/auth/authorize/"
	// TokenURL serves the authorization_code and refresh_token grants (form encoded).
	TokenURL = "https://open.tiktokapis.com/v2/oauth/token/"
	// RevokeURL ends a grant, so a disconnected account no longer lists MarketFlow.
	RevokeURL = "https://open.tiktokapis.com/v2/oauth/revoke/"
	// OpenAPIBase is the host of user info and the Content Posting API.
	OpenAPIBase = "https://open.tiktokapis.com"

	// ProviderType is the platform ProviderSetting provider_type of the TikTok developer app,
	// and the provider type the integrations page and the disconnect route use.
	ProviderType = "tiktok"
	// AccountCreator is the ConnectedAccount type of a connected TikTok account.
	AccountCreator = "tiktok_creator"

	// MetaAccessExpiresAt and MetaRefreshExpiresAt are ConnectedAccount metadata keys (RFC 3339).
	MetaAccessExpiresAt  = "access_expires_at"
	MetaRefreshExpiresAt = "refresh_expires_at"
)

// ConnectScopes are what posting needs: the profile to name the account, video.publish for
// Direct Post and video.upload for drafts sent to the creator's inbox. Nothing else is asked for.
var ConnectScopes = []string{"user.info.basic", "video.publish", "video.upload"}

// App is the platform TikTok developer app.
type App struct {
	Key         string
	Secret      string
	RedirectURL string
}

// LoadApp reads the platform app from the provider settings store. Secrets are decrypted (and a
// legacy plaintext row re-sealed) by SettingValue.
func LoadApp(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider) (App, error) {
	rows, err := db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ("platform"),
			providersetting.ProviderTypeEQ(ProviderType),
			providersetting.ProviderNameEQ("oauth"),
			providersetting.IsActiveEQ(true),
		).
		All(ctx)
	if err != nil {
		return App{}, err
	}
	var a App
	for _, r := range rows {
		v := r.Value
		if r.IsSecret || r.IsEncrypted {
			if keys == nil {
				return App{}, errors.New("tiktok app: no encryption key to read the client secret")
			}
			if v, err = keys.SettingValue(ctx, db, r); err != nil {
				return App{}, fmt.Errorf("tiktok app: decrypt %s: %w", r.Key, err)
			}
		}
		v = strings.TrimSpace(v)
		switch r.Key {
		case "client_key":
			a.Key = v
		case "client_secret":
			a.Secret = v
		case "redirect_url":
			a.RedirectURL = v
		}
	}
	if a.Key == "" || a.Secret == "" || a.RedirectURL == "" {
		return App{}, errors.New("tiktok app not configured (client_key, client_secret and redirect_url)")
	}
	return a, nil
}

// AuthCodeURL is the consent page for the scopes. Web apps do not send PKCE.
func AuthCodeURL(a App, scopes []string, state string) string {
	q := url.Values{
		"client_key":    {a.Key},
		"response_type": {"code"},
		"scope":         {strings.Join(scopes, ",")},
		"redirect_uri":  {a.RedirectURL},
		"state":         {state},
	}
	return AuthorizeURL + "?" + q.Encode()
}

// Token is a token endpoint answer.
type Token struct {
	Access           string
	Refresh          string
	ExpiresIn        time.Duration
	RefreshExpiresIn time.Duration
	OpenID           string
	Scopes           []string // granted scopes; a person may untick some on the consent page
}

// TokenError is an error answer from the token endpoint.
type TokenError struct {
	Status      int
	Code        string // invalid_grant, invalid_client, invalid_request, ...
	Description string
	LogID       string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("tiktok token %d %s: %s (log_id %s)", e.Status, e.Code, e.Description, e.LogID)
}

// Revoked says the grant no longer works (the person removed MarketFlow, or the refresh token
// expired). Only connecting again fixes it.
func (e *TokenError) Revoked() bool { return e.Code == "invalid_grant" }

// OAuth calls the token, revoke and user info endpoints. The zero value uses TikTok's own.
type OAuth struct {
	HTTP      *http.Client
	TokenURL  string
	RevokeURL string
	APIBase   string
}

func (o OAuth) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Exchange trades the callback's code for tokens.
func (o OAuth) Exchange(ctx context.Context, a App, code string) (Token, error) {
	return o.token(ctx, url.Values{
		"client_key":    {a.Key},
		"client_secret": {a.Secret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {a.RedirectURL},
	})
}

// Refresh gets a new access token. TikTok may rotate the refresh token, so the caller stores
// the one returned.
func (o OAuth) Refresh(ctx context.Context, a App, refreshToken string) (Token, error) {
	return o.token(ctx, url.Values{
		"client_key":    {a.Key},
		"client_secret": {a.Secret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (o OAuth) token(ctx context.Context, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, or(o.TokenURL, TokenURL), strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := o.client().Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("tiktok token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("tiktok token: %w", err)
	}
	var out struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_expires_in"`
		OpenID           string `json:"open_id"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		LogID            string `json:"log_id"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Error != "" && resp.StatusCode < 500 {
		return Token{}, &TokenError{Status: resp.StatusCode, Code: out.Error, Description: out.ErrorDescription, LogID: out.LogID}
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		// 5xx or an unreadable answer: worth another try later, never a reason to disconnect.
		return Token{}, fmt.Errorf("tiktok token: status %d", resp.StatusCode)
	}
	return Token{
		Access:           out.AccessToken,
		Refresh:          out.RefreshToken,
		ExpiresIn:        time.Duration(out.ExpiresIn) * time.Second,
		RefreshExpiresIn: time.Duration(out.RefreshExpiresIn) * time.Second,
		OpenID:           out.OpenID,
		Scopes:           SplitScopes(out.Scope),
	}, nil
}

// SplitScopes reads TikTok's comma separated scope list.
func SplitScopes(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// HasScopes reports whether every wanted scope was granted.
func HasScopes(granted, want []string) bool {
	have := map[string]bool{}
	for _, g := range granted {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// Revoke ends the grant at TikTok. Revoking the access token removes the whole authorization.
func (o OAuth) Revoke(ctx context.Context, a App, token string) error {
	form := url.Values{"client_key": {a.Key}, "client_secret": {a.Secret}, "token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, or(o.RevokeURL, RevokeURL), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return fmt.Errorf("tiktok revoke: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		// 400/401: the token is already invalid, which is the outcome we want.
		return nil
	}
	return fmt.Errorf("tiktok revoke: status %d", resp.StatusCode)
}

// User is the basic profile of the connected account.
type User struct {
	OpenID      string `json:"open_id"`
	UnionID     string `json:"union_id"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url"`
}

// UserInfo reads the account the token acts for (user.info.basic).
func (o OAuth) UserInfo(ctx context.Context, accessToken string) (User, error) {
	u := strings.TrimSuffix(or(o.APIBase, OpenAPIBase), "/") + "/v2/user/info/?fields=" + url.QueryEscape("open_id,union_id,avatar_url,display_name")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := o.client().Do(req)
	if err != nil {
		return User{}, fmt.Errorf("tiktok user: %w", err)
	}
	defer resp.Body.Close()
	var env struct {
		Data struct {
			User User `json:"user"`
		} `json:"data"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			LogID   string `json:"log_id"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return User{}, fmt.Errorf("tiktok user: status %d", resp.StatusCode)
	}
	if env.Error.Code != "" && env.Error.Code != "ok" {
		return User{}, fmt.Errorf("tiktok user: %s: %s (log_id %s)", env.Error.Code, env.Error.Message, env.Error.LogID)
	}
	if resp.StatusCode >= 300 || env.Data.User.OpenID == "" {
		return User{}, fmt.Errorf("tiktok user: status %d, no open_id", resp.StatusCode)
	}
	return env.Data.User, nil
}
