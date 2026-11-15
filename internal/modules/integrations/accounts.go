package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Platforms and account types of ConnectedAccount rows.
const (
	PlatformMeta   = "meta"
	PlatformTikTok = "tiktok"
	PlatformGoogle = "google"

	AccountFacebookPage      = "facebook_page"
	AccountInstagramBusiness = "instagram_business"
	AccountMetaAdAccount     = "meta_ad_account"
	AccountTikTokAdvertiser  = "tiktok_advertiser"
	AccountYouTubeChannel    = "youtube_channel"
)

// providerPlatforms maps a ProviderSetting provider_type to the ConnectedAccount platform it
// connects, so a disconnect revokes both.
var providerPlatforms = map[string]string{"meta_ads": PlatformMeta, "tiktok_ads": PlatformTikTok, providerYouTube: PlatformGoogle}

// providerAccountTypes narrows a disconnect to the provider's own account types where one
// platform has several connections: disconnecting YouTube must not touch a Google Ads customer.
var providerAccountTypes = map[string][]string{providerYouTube: {AccountYouTubeChannel}}

// accountOnlyProviders keep no tenant ProviderSetting rows: their ConnectedAccount rows are the
// whole connection.
var accountOnlyProviders = []string{providerYouTube}

// account is one external asset found at connect time.
type account struct {
	Platform    string
	Type        string
	ExternalID  string
	DisplayName string
	Token       string
	Refresh     string // a refresh token (Google), stored as ciphertext like the token
	Scopes      []string
	Metadata    map[string]any
}

// saveAccounts upserts one ConnectedAccount row per asset, in one statement. Tokens are stored
// only as ciphertext: with no encryption key configured the rows are not written at all (the
// ProviderSetting copy, re-sealed once a key exists, still serves reads), never in plain text.
// authUserID is the auth user who started the connect (from the signed state), recorded as
// connected_by when they have a MarketFlow user in this tenant.
func (h *Handler) saveAccounts(ctx context.Context, tenantID, authUserID string, accounts []account) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil || len(accounts) == 0 {
		return err
	}
	if h.keys == nil || !h.keys.Configured(ctx) {
		h.logger.Warn("connected accounts not stored: no encryption key configured", zap.String("tenant", tenantID))
		return nil
	}
	connectedBy := h.localUserID(ctx, tid, authUserID)
	stamp := time.Now().UTC().Format(time.RFC3339)
	builders := make([]*ent.ConnectedAccountCreate, 0, len(accounts))
	for _, a := range accounts {
		// connected_at tells the integrations page that publishing errors recorded before this
		// connect were about the old token (account_health.go).
		md := make(map[string]any, len(a.Metadata)+1)
		for k, v := range a.Metadata {
			md[k] = v
		}
		md[metaConnectedAt] = stamp
		b := h.db.ConnectedAccount.Create().
			SetTenantID(tid).
			SetPlatform(a.Platform).
			SetAccountType(a.Type).
			SetExternalID(a.ExternalID).
			SetDisplayName(truncate(a.DisplayName, 255)).
			SetStatus("active").
			SetNillableConnectedBy(connectedBy).
			SetMetadata(md)
		if a.Scopes != nil {
			b.SetScopes(a.Scopes)
		}
		if a.Token != "" {
			sealed, err := h.keys.Encrypt(ctx, a.Token)
			if err != nil {
				return fmt.Errorf("seal %s token: %w", a.Type, err)
			}
			b.SetTokenCiphertext(sealed)
		}
		if a.Refresh != "" {
			sealed, err := h.keys.Encrypt(ctx, a.Refresh)
			if err != nil {
				return fmt.Errorf("seal %s refresh token: %w", a.Type, err)
			}
			b.SetRefreshCiphertext(sealed)
		}
		builders = append(builders, b)
	}
	return h.db.ConnectedAccount.CreateBulk(builders...).
		OnConflictColumns(
			connectedaccount.FieldTenantID,
			connectedaccount.FieldPlatform,
			connectedaccount.FieldAccountType,
			connectedaccount.FieldExternalID,
		).
		UpdateNewValues().
		// A reconnect brings a new token, so the old expiry no longer applies; the token health
		// job finds the row due (no expiry) and records the new one on its next run.
		Update(func(u *ent.ConnectedAccountUpsert) { u.ClearExpiresAt() }).
		Exec(ctx)
}

// revokeAccounts marks a platform's accounts revoked and drops their tokens, which are useless
// after a disconnect and should not be kept.
func (h *Handler) revokeAccounts(ctx context.Context, tenantID, provider string) (int, error) {
	platform, ok := providerPlatforms[provider]
	tid, err := uuid.Parse(tenantID)
	if !ok || err != nil {
		return 0, nil
	}
	u := h.db.ConnectedAccount.Update().
		Where(
			connectedaccount.TenantID(tid),
			connectedaccount.Platform(platform),
			connectedaccount.StatusNEQ("revoked"),
		)
	if types := providerAccountTypes[provider]; len(types) > 0 {
		u = u.Where(connectedaccount.AccountTypeIn(types...))
	}
	return u.
		SetStatus("revoked").
		ClearTokenCiphertext().
		ClearRefreshCiphertext().
		Save(ctx)
}

// localUserID maps an auth user id to the tenant's MarketFlow user id (nil when unknown).
func (h *Handler) localUserID(ctx context.Context, tenantID uuid.UUID, authUserID string) *uuid.UUID {
	aid, err := uuid.Parse(authUserID)
	if err != nil {
		return nil
	}
	u, err := h.db.MarketflowUser.Query().
		Where(marketflowuser.TenantID(tenantID), marketflowuser.AuthServiceUserID(aid)).
		Only(ctx)
	if err != nil {
		return nil
	}
	return &u.ID
}

// requestUserID is the signed-in auth user's id, "" for an API key caller.
func requestUserID(r *http.Request) string {
	c, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || c == nil {
		return ""
	}
	id, err := c.UserID()
	if err != nil {
		return ""
	}
	return id.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ── Meta Graph reads at connect time ─────────────────────────────────────────

const metaGraphURL = meta.GraphURL

var graphClient = &http.Client{Timeout: 20 * time.Second}

type metaPage struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	AccessToken string `json:"access_token"`
	Instagram   *metaIGAccount `json:"instagram_business_account"`
}

type metaAdAccount struct {
	ID            string `json:"id"` // act_<id>
	Name          string `json:"name"`
	Currency      string `json:"currency"`
	AccountStatus int    `json:"account_status"`
}

// graphList follows a Graph list endpoint's paging up to maxPages pages.
func graphList[T any](ctx context.Context, path string, q url.Values, maxPages int) ([]T, error) {
	next := metaGraphURL + path + "?" + q.Encode()
	var out []T
	for page := 0; next != "" && page < maxPages; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return out, err
		}
		resp, err := graphClient.Do(req)
		if err != nil {
			return out, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return out, err
		}
		var res struct {
			Data   []T `json:"data"`
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return out, fmt.Errorf("graph %s: %w", path, err)
		}
		if res.Error != nil {
			return out, fmt.Errorf("graph %s: %s", path, res.Error.Message)
		}
		out = append(out, res.Data...)
		next = res.Paging.Next
	}
	return out, nil
}

// metaUserID returns the app-scoped id of the person who granted the token (best
// effort: "" on failure). Meta's deletion and deauthorize callbacks name people by it.
func metaUserID(ctx context.Context, userToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		metaGraphURL+"/me?"+url.Values{"fields": {"id"}, "access_token": {userToken}}.Encode(), nil)
	if err != nil {
		return ""
	}
	resp, err := graphClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var me struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&me) != nil {
		return ""
	}
	return me.ID
}

// metaGrantedScopes lists the permissions the user granted (best effort: nil on failure).
func metaGrantedScopes(ctx context.Context, userToken string) []string {
	perms, err := graphList[struct {
		Permission string `json:"permission"`
		Status     string `json:"status"`
	}](ctx, "/me/permissions", url.Values{"access_token": {userToken}}, 1)
	if err != nil {
		return nil
	}
	scopes := make([]string, 0, len(perms))
	for _, p := range perms {
		if p.Status == "granted" {
			scopes = append(scopes, p.Permission)
		}
	}
	return scopes
}

// metaAccounts turns the granted Pages, their Instagram business accounts and the user's ad
// accounts into ConnectedAccount rows. Pages and Instagram accounts act with the Page token; ad
// accounts with the user token.
func metaAccounts(pages []metaPage, adAccounts []metaAdAccount, userToken string, scopes []string, fbUserID string) []account {
	out := make([]account, 0, len(pages)*2+len(adAccounts))
	// Every row records who granted it, for Meta's deletion callback (meta_deletion.go).
	meta := func(m map[string]any) map[string]any {
		if fbUserID != "" {
			m[MetaFBUserKey] = fbUserID
		}
		return m
	}
	for _, p := range pages {
		if p.ID == "" {
			continue
		}
		out = append(out, account{
			Platform: PlatformMeta, Type: AccountFacebookPage, ExternalID: p.ID, DisplayName: p.Name,
			Token: p.AccessToken, Scopes: scopes, Metadata: meta(map[string]any{}),
		})
		if p.Instagram != nil && p.Instagram.ID != "" {
			out = append(out, account{
				Platform: PlatformMeta, Type: AccountInstagramBusiness, ExternalID: p.Instagram.ID, DisplayName: p.Instagram.Username,
				Token: p.AccessToken, Scopes: scopes, Metadata: meta(map[string]any{"page_id": p.ID, "picture_url": p.Instagram.Picture}),
			})
		}
	}
	for _, a := range adAccounts {
		if a.ID == "" {
			continue
		}
		out = append(out, account{
			Platform: PlatformMeta, Type: AccountMetaAdAccount, ExternalID: a.ID, DisplayName: a.Name,
			Token: userToken, Scopes: scopes,
			Metadata: meta(map[string]any{"currency": a.Currency, "account_status": a.AccountStatus}),
		})
	}
	return out
}

// metaIGAccount is a Page's linked Instagram professional account.
type metaIGAccount struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Picture  string `json:"profile_picture_url"`
}
