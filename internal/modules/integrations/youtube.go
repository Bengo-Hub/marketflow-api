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
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
)

// YouTube channel connection (Sprint 16). The tenant signs in with the Google account that owns
// the channel; MarketFlow keeps the refresh token as ciphertext on the channel's ConnectedAccount
// row and renews the one-hour access token before each upload or delete (google.Tokens). The
// scopes are publishing.YouTubeConnectScopes: youtube.upload to post, and youtube.force-ssl to
// delete what MarketFlow posted and to read which channel was connected. No other Google data
// is asked for.

// providerYouTube is the provider type the integrations page and the disconnect route use.
const providerYouTube = google.ProviderYouTube

// youtubeAccounts turns the channels the token acts for into ConnectedAccount rows. Each row
// carries the same grant: the access token now, the refresh token for later.
func youtubeAccounts(chans []google.Channel, tok google.Token, now time.Time) []account {
	out := make([]account, 0, len(chans))
	for _, c := range chans {
		md := map[string]any{
			google.MetaAccessExpiresAt: now.Add(tok.ExpiresIn).UTC().Format(time.RFC3339),
		}
		if c.Thumbnail != "" {
			md["picture_url"] = c.Thumbnail
		}
		if c.CustomURL != "" {
			md["handle"] = c.CustomURL
		}
		out = append(out, account{
			Platform: PlatformGoogle, Type: AccountYouTubeChannel, ExternalID: c.ID, DisplayName: c.Title,
			Token: tok.Access, Refresh: tok.Refresh, Scopes: tok.Scopes, Metadata: md,
		})
	}
	return out
}

// YouTubeAuthorize returns the Google consent URL for connecting a YouTube channel.
// @Summary Start the YouTube channel connection for a tenant
// @Tags Integrations
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]string
// @Failure 503 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /integrations/youtube/authorize [get]
func (h *Handler) YouTubeAuthorize(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := h.resolveTenantID(r)
	if tenantID == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant",
			"no tenant context; pass X-Tenant-ID or X-Tenant-Slug", nil)
		return
	}
	c, err := google.LoadYouTubeClient(r.Context(), h.db, h.keys)
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"platform YouTube OAuth client not configured", nil)
		return
	}
	state, ok := h.issueStateOrFail(w, r, tenantID)
	if !ok {
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]string{
		"url": google.AuthCodeURL(c, publishing.YouTubeConnectScopes, state),
	})
}

// YouTubeCallback exchanges the code, reads the channel and stores it as a ConnectedAccount.
// @Summary YouTube channel OAuth callback
// @Tags Integrations
// @Produce html
// @Param code query string true "Authorization code"
// @Param state query string true "Signed state"
// @Router /integrations/youtube/callback [get]
func (h *Handler) YouTubeCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID, userID, stErr := h.redeemState(w, r, r.URL.Query().Get("state"))
	if stErr != nil {
		h.failConnect(w, r, "youtube", tenantID, stateReason(stErr), stErr.Error())
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		// The person cancelled on Google's page (error=access_denied) or it sent nothing.
		h.failConnect(w, r, "youtube", tenantID, "cancelled", r.URL.Query().Get("error"))
		return
	}
	// The tokens live only as ciphertext on the ConnectedAccount row, which saveAccounts never
	// writes without a key; without one the connect would look done and store nothing.
	if h.keys == nil || !h.keys.Configured(ctx) {
		h.failConnect(w, r, "youtube", tenantID, "oauth_not_configured", "no encryption key configured")
		return
	}
	c, err := google.LoadYouTubeClient(ctx, h.db, h.keys)
	if err != nil {
		h.failConnect(w, r, "youtube", tenantID, "oauth_not_configured", err.Error())
		return
	}
	tok, err := h.googleOAuth.Exchange(ctx, c, code)
	if err != nil {
		h.failConnect(w, r, "youtube", tenantID, "token_exchange_failed", err.Error())
		return
	}
	if tok.Refresh == "" {
		h.failConnect(w, r, "youtube", tenantID, "no_refresh_token", "Google returned no refresh_token")
		return
	}
	// Google's consent page lets a person untick a permission. Without upload nothing can be
	// posted, and without force-ssl the channel cannot be read or a post deleted, so the grant
	// is handed back rather than stored half working.
	if !google.HasScopes(tok.Scopes, publishing.YouTubeConnectScopes) {
		h.revokeGrant(ctx, tenantID, tok.Refresh)
		h.failConnect(w, r, "youtube", tenantID, "missing_scope", "granted: "+strings.Join(tok.Scopes, " "))
		return
	}
	chans, err := google.MyChannels(ctx, h.httpClient(), h.youtubeAPIBase, tok.Access)
	if err != nil {
		h.revokeGrant(ctx, tenantID, tok.Refresh)
		h.failConnect(w, r, "youtube", tenantID, "channel_read_failed", err.Error())
		return
	}
	if len(chans) == 0 {
		h.revokeGrant(ctx, tenantID, tok.Refresh)
		h.failConnect(w, r, "youtube", tenantID, "no_channel", "the Google account has no YouTube channel")
		return
	}
	accs := youtubeAccounts(chans, tok, time.Now())
	if err := h.saveAccounts(ctx, tenantID, userID, accs); err != nil {
		h.failConnect(w, r, "youtube", tenantID, "persist_failed", err.Error())
		return
	}
	// A channel the person no longer picks on Google's page is retired.
	_, _ = h.revokeMissing(ctx, tenantID, PlatformGoogle, []string{AccountYouTubeChannel}, externalIDsOf(accs, AccountYouTubeChannel))
	target := h.integrationsURL(ctx, tenantID, "youtube=connected&channel="+url.QueryEscape(chans[0].Title))
	http.Redirect(w, r, target, http.StatusFound)
}

// revokeGrant hands a grant back to Google, best effort: a failed revoke is only logged.
func (h *Handler) revokeGrant(ctx context.Context, tenantID, token string) {
	if token == "" {
		return
	}
	if err := h.googleOAuth.Revoke(ctx, token); err != nil {
		h.logger.Info("youtube grant not revoked", zap.String("tenant", tenantID), zap.Error(err))
	}
}

// revokeGoogleGrants revokes each connected Google account's grant (YouTube channels, Google Ads
// accounts) before a disconnect drops the tokens, so MarketFlow leaves the person's Google
// third-party access list.
func (h *Handler) revokeGoogleGrants(ctx context.Context, tenantID, accountType string) {
	tid, err := uuid.Parse(tenantID)
	if err != nil || h.keys == nil {
		return
	}
	rows, err := h.db.ConnectedAccount.Query().
		Where(
			connectedaccount.TenantIDEQ(tid),
			connectedaccount.AccountTypeEQ(accountType),
			connectedaccount.StatusNEQ("revoked"),
			connectedaccount.RefreshCiphertextNEQ(""),
		).
		All(ctx)
	if err != nil {
		h.logger.Warn("youtube disconnect: channels not read for revoke", zap.String("tenant", tenantID), zap.Error(err))
		return
	}
	for _, a := range rows {
		rt, err := h.keys.Decrypt(ctx, a.RefreshCiphertext)
		if err != nil || rt == "" {
			continue
		}
		h.revokeGrant(ctx, tenantID, rt)
	}
}

func (h *Handler) httpClient() *http.Client {
	if h.googleOAuth.HTTP != nil {
		return h.googleOAuth.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}
