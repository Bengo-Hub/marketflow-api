package integrations

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/integrations/tiktok"
)

// TikTok account connection for organic posting (Login Kit for Web plus the Content Posting
// API). The business signs in with its own TikTok account; MarketFlow keeps the access and
// refresh tokens as ciphertext on the account's ConnectedAccount row (type tiktok_creator,
// external id = open_id) and renews the 24 hour access token before each post (tiktok.Tokens).
// The scopes are tiktok.ConnectScopes. TikTok for Business (ads and leads, tiktok_ads) is a
// separate app and connection.

const (
	providerTikTok       = tiktok.ProviderType
	AccountTikTokCreator = tiktok.AccountCreator
	// connectTikTok is the query key the callback answers with (?tiktok_creator=connected).
	connectTikTok = "tiktok_creator"
)

// tiktokCreatorAccount turns the token and profile into the ConnectedAccount row.
func tiktokCreatorAccount(u tiktok.User, tok tiktok.Token, now time.Time) account {
	md := map[string]any{tiktok.MetaAccessExpiresAt: now.Add(tok.ExpiresIn).UTC().Format(time.RFC3339)}
	if tok.RefreshExpiresIn > 0 {
		md[tiktok.MetaRefreshExpiresAt] = now.Add(tok.RefreshExpiresIn).UTC().Format(time.RFC3339)
	}
	if u.AvatarURL != "" {
		md["picture_url"] = u.AvatarURL
	}
	if u.UnionID != "" {
		md["union_id"] = u.UnionID
	}
	name := u.DisplayName
	if name == "" {
		name = "TikTok account"
	}
	return account{Platform: PlatformTikTok, Type: AccountTikTokCreator, ExternalID: u.OpenID, DisplayName: name,
		Token: tok.Access, Refresh: tok.Refresh, Scopes: tok.Scopes, Metadata: md}
}

// TikTokCreatorAuthorize returns the TikTok consent URL for connecting the business's account.
// @Summary Start the TikTok account connection (posting) for a tenant
// @Tags Integrations
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]string
// @Failure 503 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /integrations/tiktok-creator/authorize [get]
func (h *Handler) TikTokCreatorAuthorize(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := h.resolveTenantID(r)
	if tenantID == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant",
			"no tenant context; pass X-Tenant-ID or X-Tenant-Slug", nil)
		return
	}
	app, err := tiktok.LoadApp(r.Context(), h.db, h.keys)
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"platform TikTok app not configured", nil)
		return
	}
	state, ok := h.issueStateOrFail(w, r, tenantID)
	if !ok {
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]string{"url": tiktok.AuthCodeURL(app, tiktok.ConnectScopes, state)})
}

// TikTokCreatorCallback exchanges the code, reads the account and stores it as a ConnectedAccount.
// @Summary TikTok account OAuth callback (posting)
// @Tags Integrations
// @Produce html
// @Param code query string true "Authorization code"
// @Param state query string true "Signed state"
// @Router /integrations/tiktok-creator/callback [get]
func (h *Handler) TikTokCreatorCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID, userID, stErr := h.redeemState(w, r, r.URL.Query().Get("state"))
	if stErr != nil {
		h.failConnect(w, r, connectTikTok, tenantID, stateReason(stErr), stErr.Error())
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		// The person cancelled on TikTok's page (error=access_denied) or it sent nothing.
		h.failConnect(w, r, connectTikTok, tenantID, "cancelled", r.URL.Query().Get("error"))
		return
	}
	if h.keys == nil || !h.keys.Configured(ctx) {
		h.failConnect(w, r, connectTikTok, tenantID, "oauth_not_configured", "no encryption key configured")
		return
	}
	app, err := tiktok.LoadApp(ctx, h.db, h.keys)
	if err != nil {
		h.failConnect(w, r, connectTikTok, tenantID, "oauth_not_configured", err.Error())
		return
	}
	tok, err := h.tiktokOAuth.Exchange(ctx, app, code)
	if err != nil {
		h.failConnect(w, r, connectTikTok, tenantID, "token_exchange_failed", err.Error())
		return
	}
	if tok.Refresh == "" {
		h.revokeTikTok(ctx, tenantID, app, tok.Access)
		h.failConnect(w, r, connectTikTok, tenantID, "no_refresh_token", "TikTok returned no refresh_token")
		return
	}
	// TikTok's consent page lets a person untick a permission. Without video.publish nothing can
	// be posted, so the grant is handed back rather than stored half working.
	if !tiktok.HasScopes(tok.Scopes, tiktok.ConnectScopes) {
		h.revokeTikTok(ctx, tenantID, app, tok.Access)
		h.failConnect(w, r, connectTikTok, tenantID, "missing_scope", "granted: "+strings.Join(tok.Scopes, ","))
		return
	}
	user, err := h.tiktokOAuth.UserInfo(ctx, tok.Access)
	if err != nil {
		h.revokeTikTok(ctx, tenantID, app, tok.Access)
		h.failConnect(w, r, connectTikTok, tenantID, "account_read_failed", err.Error())
		return
	}
	acc := tiktokCreatorAccount(user, tok, time.Now())
	if err := h.saveAccounts(ctx, tenantID, userID, []account{acc}); err != nil {
		h.failConnect(w, r, connectTikTok, tenantID, "persist_failed", err.Error())
		return
	}
	// One grant is one TikTok account: other connected TikTok accounts of the business stay.
	target := h.integrationsURL(ctx, tenantID, connectTikTok+"=connected&account="+url.QueryEscape(acc.DisplayName))
	http.Redirect(w, r, target, http.StatusFound)
}

// revokeTikTok hands a grant back to TikTok, best effort: a failed revoke is only logged.
func (h *Handler) revokeTikTok(ctx context.Context, tenantID string, app tiktok.App, token string) {
	if token == "" {
		return
	}
	if err := h.tiktokOAuth.Revoke(ctx, app, token); err != nil {
		h.logger.Info("tiktok grant not revoked", zap.String("tenant", tenantID), zap.Error(err))
	}
}

// revokeTikTokGrants revokes each connected TikTok account's grant before a disconnect drops the
// tokens, so MarketFlow leaves the person's TikTok authorized apps.
func (h *Handler) revokeTikTokGrants(ctx context.Context, tenantID string) {
	tid, err := uuid.Parse(tenantID)
	if err != nil || h.keys == nil {
		return
	}
	app, err := tiktok.LoadApp(ctx, h.db, h.keys)
	if err != nil {
		return
	}
	rows, err := h.db.ConnectedAccount.Query().
		Where(
			connectedaccount.TenantIDEQ(tid),
			connectedaccount.AccountTypeEQ(AccountTikTokCreator),
			connectedaccount.StatusNEQ("revoked"),
			connectedaccount.TokenCiphertextNEQ(""),
		).
		All(ctx)
	if err != nil {
		h.logger.Warn("tiktok disconnect: accounts not read for revoke", zap.String("tenant", tenantID), zap.Error(err))
		return
	}
	for _, a := range rows {
		tok, err := h.keys.Decrypt(ctx, a.TokenCiphertext)
		if err != nil || tok == "" {
			continue
		}
		h.revokeTikTok(ctx, tenantID, app, tok)
	}
}
