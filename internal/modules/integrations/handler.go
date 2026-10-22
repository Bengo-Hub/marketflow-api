// Package integrations implements per-tenant OAuth flows for Google Calendar
// and Meta. Platform OAuth client credentials live in ProviderSetting with
// tenant_id="platform"; per-tenant refresh_tokens / page_tokens are written
// back to ProviderSetting with tenant_id=<uuid>.
//
// Flow:
//  1. Tenant admin clicks "Connect" in /[orgSlug]/settings/integrations.
//  2. UI redirects to /api/v1/integrations/{provider}/authorize?tenant_id=<uuid>.
//  3. Backend builds the provider's consent URL with a short-lived signed
//     `state` carrying {tenant_id, nonce}, signed with MF_SECURITY_ENCRYPTION_KEY.
//  4. Provider redirects to /api/v1/integrations/{provider}/callback.
//  5. Backend verifies state, exchanges code, persists the token as ProviderSetting and
//     writes one ConnectedAccount row per Page, Instagram account, ad account or TikTok
//     advertiser granted (tokens as ciphertext only; see accounts.go).
package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	chirouter "github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler owns the OAuth endpoints.
type Handler struct {
	db       *ent.Client
	stateKey []byte                  // signs the `state` round-trip; reuses encryption key
	keys     *encryption.KeyProvider // encrypts OAuth tokens at rest, decrypts platform secrets
	logger   *zap.Logger
}

// NewHandler constructs a Handler.
func NewHandler(db *ent.Client, stateKey []byte, keys *encryption.KeyProvider, logger *zap.Logger) *Handler {
	return &Handler{db: db, stateKey: stateKey, keys: keys, logger: logger.Named("integrations")}
}

// resolveTenantID picks a tenant context using, in order of priority:
//  1. X-Tenant-ID header (typical SSO tenant user, explicit)
//  2. X-Tenant-Slug header → DB lookup (platform owner acting as a tenant
//     via the /{orgSlug}/... UI routes)
//  3. JWT claims tenant_id / tenant slug (fallback when headers absent)
//
// Returns the UUID as a string + a flag whether the caller was acting as
// platform owner. Empty string when no tenant could be resolved.
func (h *Handler) resolveTenantID(r *http.Request) (string, bool) {
	if v := strings.TrimSpace(r.Header.Get("X-Tenant-ID")); v != "" {
		return v, false
	}
	ctx := r.Context()
	claims, _ := authclient.ClaimsFromContext(ctx)
	isPO := claims != nil && (claims.IsPlatformOwner || claims.GetTenantSlug() == "codevertex")

	if slug := strings.TrimSpace(r.Header.Get("X-Tenant-Slug")); slug != "" {
		if t, err := h.db.Tenant.Query().Where(tenant.SlugEQ(slug)).Only(ctx); err == nil {
			return t.ID.String(), isPO
		}
	}
	// Last resort: tenant from JWT claims.
	if claims != nil && claims.TenantID != "" {
		return claims.TenantID, isPO
	}
	return "", isPO
}

// ── Google Calendar ─────────────────────────────────────────────────────────

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleScope    = "https://www.googleapis.com/auth/calendar.events https://www.googleapis.com/auth/calendar.freebusy"
)

// GoogleAuthorize returns the consent URL for the tenant to visit.
// @Summary Start Google Calendar OAuth for a tenant
// @Tags Integrations
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /integrations/google/authorize [get]
func (h *Handler) GoogleAuthorize(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := h.resolveTenantID(r)
	if tenantID == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant",
			"no tenant context; pass X-Tenant-ID or X-Tenant-Slug", nil)
		return
	}
	creds, err := h.loadPlatform(r.Context(), "google_calendar", "oauth")
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"platform google_calendar oauth client not configured", nil)
		return
	}
	state := h.signState(tenantID, requestUserID(r))
	q := url.Values{
		"client_id":     {creds["client_id"]},
		"redirect_uri":  {creds["redirect_url"]},
		"response_type": {"code"},
		"scope":         {googleScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	httpapi.JSON(w, http.StatusOK, map[string]string{
		"url": googleAuthURL + "?" + q.Encode(),
	})
}

// GoogleCallback is the OAuth redirect URI. Exchanges code -> refresh_token
// and persists it as a tenant ProviderSetting row. Redirects the browser to
// the tenant's integrations page with a success/error flag.
// @Summary Google Calendar OAuth callback
// @Tags Integrations
// @Produce html
// @Param code query string true "Authorization code"
// @Param state query string true "Signed state"
// @Router /integrations/google/callback [get]
func (h *Handler) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_params", "code and state required", nil)
		return
	}
	tenantID, _, ok := h.verifyState(state)
	if !ok {
		httpapi.Error(w, http.StatusUnauthorized, "bad_state", "state signature invalid", nil)
		return
	}
	creds, err := h.loadPlatform(r.Context(), "google_calendar", "oauth")
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "oauth_not_configured", err.Error(), nil)
		return
	}

	form := url.Values{
		"code":          {code},
		"client_id":     {creds["client_id"]},
		"client_secret": {creds["client_secret"]},
		"redirect_uri":  {creds["redirect_url"]},
		"grant_type":    {"authorization_code"},
	}
	resp, err := http.PostForm(googleTokenURL, form)
	if err != nil {
		httpapi.Error(w, http.StatusBadGateway, "token_exchange_failed", err.Error(), nil)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		httpapi.Error(w, http.StatusBadGateway, "token_exchange_failed",
			fmt.Sprintf("google %d: %s", resp.StatusCode, string(body)), nil)
		return
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		httpapi.Error(w, http.StatusBadGateway, "token_parse_failed", err.Error(), nil)
		return
	}
	if tok.RefreshToken == "" {
		httpapi.Error(w, http.StatusBadRequest, "no_refresh_token",
			"Google returned no refresh_token; ensure access_type=offline + prompt=consent and the tenant has not previously approved", nil)
		return
	}
	// Persist refresh_token + default calendar. calendar_id defaults to
	// "primary" and can be edited in /settings/integrations.
	if err := h.upsertTenantKV(r.Context(), tenantID, "google_calendar", "oauth", map[string]string{
		"refresh_token": tok.RefreshToken,
		"calendar_id":   "primary",
	}, []string{"refresh_token"}); err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "persist_failed", err.Error(), nil)
		return
	}
	// Redirect back to the dashboard integrations page with a success flag.
	target := h.integrationsURL(r.Context(), tenantID, "google=connected")
	http.Redirect(w, r, target, http.StatusFound)
}

// ── Meta ────────────────────────────────────────────────────────────────────

const (
	metaAuthURL  = "https://www.facebook.com/v21.0/dialog/oauth"
	metaTokenURL = "https://graph.facebook.com/v21.0/oauth/access_token"
	metaScope    = "pages_show_list,pages_read_engagement,leads_retrieval,pages_manage_metadata"
)

// metaLoginParams sets what the consent dialog asks for, from the platform Meta app settings.
// Meta refuses a dialog that asks for a permission the app has not been approved for (for
// everyone but the app's own testers), so the list grows in settings as App Review approves
// permissions, never in code:
//   - login_config_id: a Facebook Login for Business configuration; its permissions and assets
//     are set in Meta's dashboard and replace the scope list;
//   - scopes: a comma-separated permission list for classic Facebook Login;
//   - neither: the permissions MarketFlow has always asked for (Pages and Lead Ads).
func metaLoginParams(q url.Values, creds map[string]string) {
	if id := strings.TrimSpace(creds["login_config_id"]); id != "" {
		q.Set("config_id", id)
		q.Set("override_default_response_type", "true")
		return
	}
	scope := metaScope
	if s := strings.Join(strings.Fields(strings.ReplaceAll(creds["scopes"], ",", " ")), ","); s != "" {
		scope = s
	}
	q.Set("scope", scope)
}

// MetaAuthorize returns the consent URL for connecting a Facebook Page.
// @Summary Start Meta OAuth for a tenant
// @Tags Integrations
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /integrations/meta/authorize [get]
func (h *Handler) MetaAuthorize(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := h.resolveTenantID(r)
	if tenantID == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant",
			"no tenant context; pass X-Tenant-ID or X-Tenant-Slug", nil)
		return
	}
	creds, err := h.loadPlatform(r.Context(), "meta_ads", "oauth")
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"platform meta oauth app not configured", nil)
		return
	}
	state := h.signState(tenantID, requestUserID(r))
	q := url.Values{
		"client_id":     {creds["app_id"]},
		"redirect_uri":  {creds["redirect_url"]},
		"response_type": {"code"},
		"state":         {state},
	}
	metaLoginParams(q, creds)
	httpapi.JSON(w, http.StatusOK, map[string]string{
		"url": metaAuthURL + "?" + q.Encode(),
	})
}

// MetaCallback exchanges the code for a long-lived user token, then lists the
// user's pages and persists the first page's access token. The tenant can
// later change which page is active via /settings/integrations.
// @Summary Meta OAuth callback
// @Tags Integrations
// @Produce html
// @Param code query string true "Authorization code"
// @Param state query string true "Signed state"
// @Router /integrations/meta/callback [get]
func (h *Handler) MetaCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_params", "code and state required", nil)
		return
	}
	tenantID, userID, ok := h.verifyState(state)
	if !ok {
		httpapi.Error(w, http.StatusUnauthorized, "bad_state", "state signature invalid", nil)
		return
	}
	creds, err := h.loadPlatform(r.Context(), "meta_ads", "oauth")
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "oauth_not_configured", err.Error(), nil)
		return
	}
	// Step 1: exchange code for short-lived user token.
	q := url.Values{
		"client_id":     {creds["app_id"]},
		"client_secret": {creds["app_secret"]},
		"redirect_uri":  {creds["redirect_url"]},
		"code":          {code},
	}
	resp, err := http.Get(metaTokenURL + "?" + q.Encode())
	if err != nil {
		httpapi.Error(w, http.StatusBadGateway, "token_exchange_failed", err.Error(), nil)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var shortTok struct {
		AccessToken string `json:"access_token"`
		Error       struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &shortTok); err != nil || shortTok.AccessToken == "" {
		httpapi.Error(w, http.StatusBadGateway, "token_exchange_failed",
			fmt.Sprintf("meta: %s", strings.TrimSpace(string(body))), nil)
		return
	}
	// Step 2: exchange for long-lived user token (60 days).
	longQ := url.Values{
		"grant_type":        {"fb_exchange_token"},
		"client_id":         {creds["app_id"]},
		"client_secret":     {creds["app_secret"]},
		"fb_exchange_token": {shortTok.AccessToken},
	}
	resp2, err := http.Get(metaTokenURL + "?" + longQ.Encode())
	if err != nil {
		httpapi.Error(w, http.StatusBadGateway, "long_token_failed", err.Error(), nil)
		return
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	var longTok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(body2, &longTok)
	userToken := longTok.AccessToken
	if userToken == "" {
		userToken = shortTok.AccessToken
	}
	// Step 3: fetch every Page the user granted, with its Instagram business account. The first
	// is the tenant's default Page (switchable in the UI); all are stored as ConnectedAccounts.
	pages, err := graphList[metaPage](r.Context(), "/me/accounts", url.Values{
		"access_token": {userToken},
		"fields":       {"id,name,access_token,instagram_business_account{id,username}"},
		"limit":        {"100"},
	}, 5)
	if err != nil && len(pages) == 0 {
		httpapi.Error(w, http.StatusBadGateway, "pages_fetch_failed", err.Error(), nil)
		return
	}
	if len(pages) == 0 {
		httpapi.Error(w, http.StatusBadRequest, "no_pages",
			"no Facebook Pages found for this user", nil)
		return
	}
	p := pages[0]
	if err := h.upsertTenantKV(r.Context(), tenantID, "meta_ads", "page", map[string]string{
		"page_id":    p.ID,
		"page_name":  p.Name,
		"page_token": p.AccessToken,
		"user_token": userToken,
	}, []string{"page_token", "user_token"}); err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "persist_failed", err.Error(), nil)
		return
	}
	// The Meta Lead Ads webhook resolves the tenant by tenants.meta_page_id; without it no
	// lead from this Page could ever be attributed (Sprint 14 G2).
	if tid, perr := uuid.Parse(tenantID); perr == nil {
		if err := h.db.Tenant.UpdateOneID(tid).SetMetaPageID(p.ID).Exec(r.Context()); err != nil {
			h.logger.Warn("meta connect: could not record the page id on the tenant", zap.String("tenant", tenantID), zap.Error(err))
		}
	}
	// Ad accounts need ads_management or ads_read; without them the list is simply empty.
	adAccounts, err := graphList[metaAdAccount](r.Context(), "/me/adaccounts", url.Values{
		"access_token": {userToken},
		"fields":       {"id,name,currency,account_status"},
		"limit":        {"100"},
	}, 5)
	if err != nil {
		h.logger.Info("meta connect: ad accounts not listed", zap.String("tenant", tenantID), zap.Error(err))
	}
	accounts := metaAccounts(pages, adAccounts, userToken, metaGrantedScopes(r.Context(), userToken))
	if err := h.saveAccounts(r.Context(), tenantID, userID, accounts); err != nil {
		// The default Page is already saved above, so the connect still works; the full list
		// is stored again on the next connect.
		h.logger.Error("meta connect: connected accounts not stored", zap.String("tenant", tenantID), zap.Error(err))
	}
	target := h.integrationsURL(r.Context(), tenantID, "meta=connected&page="+url.QueryEscape(p.Name))
	http.Redirect(w, r, target, http.StatusFound)
}

// ── TikTok for Business ────────────────────────────────────────────────────

const (
	tiktokAuthURL  = "https://business-api.tiktok.com/portal/auth"
	tiktokTokenURL = "https://business-api.tiktok.com/open_api/v1.3/oauth2/access_token/"
)

// TikTokAuthorize redirects the tenant to TikTok's OAuth consent page.
// @Summary Start TikTok OAuth flow
// @Tags Integrations
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]string
// @Router /integrations/tiktok/authorize [get]
func (h *Handler) TikTokAuthorize(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := h.resolveTenantID(r)
	if tenantID == "" {
		httpapi.Error(w, http.StatusBadRequest, "no_tenant", "missing tenant context", nil)
		return
	}
	creds, err := h.loadPlatform(r.Context(), "tiktok_ads", "oauth")
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "tiktok_not_configured",
			"TikTok app credentials not configured. Set them at /platform/providers.", nil)
		return
	}
	state := h.signState(tenantID, requestUserID(r))
	authURL := fmt.Sprintf("%s?app_id=%s&redirect_uri=%s&state=%s",
		tiktokAuthURL,
		url.QueryEscape(creds["app_id"]),
		url.QueryEscape(creds["redirect_url"]),
		url.QueryEscape(state),
	)
	httpapi.JSON(w, http.StatusOK, map[string]string{"url": authURL})
}

// TikTokCallback handles the OAuth callback from TikTok.
// @Summary TikTok OAuth callback
// @Tags Integrations
// @Param auth_code query string true "Authorization code"
// @Param state query string true "Signed state"
// @Router /integrations/tiktok/callback [get]
func (h *Handler) TikTokCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("auth_code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_params", "auth_code and state required", nil)
		return
	}
	tenantID, userID, ok := h.verifyState(state)
	if !ok {
		httpapi.Error(w, http.StatusUnauthorized, "bad_state", "state signature invalid", nil)
		return
	}
	creds, err := h.loadPlatform(r.Context(), "tiktok_ads", "oauth")
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "oauth_not_configured", err.Error(), nil)
		return
	}
	// Exchange auth_code for access_token
	payload, _ := json.Marshal(map[string]string{
		"app_id":    creds["app_id"],
		"secret":    creds["app_secret"],
		"auth_code": code,
	})
	resp, err := http.Post(tiktokTokenURL, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		httpapi.Error(w, http.StatusBadGateway, "token_exchange_failed", err.Error(), nil)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var tokResp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken   string   `json:"access_token"`
			AdvertiserIDs []string `json:"advertiser_ids"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &tokResp); err != nil || tokResp.Data.AccessToken == "" {
		httpapi.Error(w, http.StatusBadGateway, "token_exchange_failed",
			fmt.Sprintf("tiktok: code=%d msg=%s", tokResp.Code, tokResp.Message), nil)
		return
	}
	advertiserID := ""
	if len(tokResp.Data.AdvertiserIDs) > 0 {
		advertiserID = tokResp.Data.AdvertiserIDs[0]
	}
	if err := h.upsertTenantKV(r.Context(), tenantID, "tiktok_ads", "oauth", map[string]string{
		"access_token":  tokResp.Data.AccessToken,
		"advertiser_id": advertiserID,
	}, []string{"access_token"}); err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "persist_failed", err.Error(), nil)
		return
	}
	// One token covers every advertiser the user authorised; each is its own account.
	advertisers := make([]account, 0, len(tokResp.Data.AdvertiserIDs))
	for _, id := range tokResp.Data.AdvertiserIDs {
		if id != "" {
			advertisers = append(advertisers, account{
				Platform: PlatformTikTok, Type: AccountTikTokAdvertiser, ExternalID: id,
				Token: tokResp.Data.AccessToken, Metadata: map[string]any{},
			})
		}
	}
	if err := h.saveAccounts(r.Context(), tenantID, userID, advertisers); err != nil {
		h.logger.Error("tiktok connect: connected accounts not stored", zap.String("tenant", tenantID), zap.Error(err))
	}
	target := h.integrationsURL(r.Context(), tenantID, "tiktok=connected")
	http.Redirect(w, r, target, http.StatusFound)
}

// ── Tenant integration management ───────────────────────────────────────────

// ListTenant returns a non-sensitive summary of the tenant's linked integrations.
// @Summary List tenant integrations
// @Tags Integrations
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /integrations [get]
func (h *Handler) ListTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, isPO := h.resolveTenantID(r)
	if tenantID == "" {
		// Platform owners with no tenant context get an empty list rather
		// than a 400 — lets the UI render the integrations page gracefully
		// without forcing them to impersonate a tenant.
		if isPO {
			httpapi.JSON(w, http.StatusOK, map[string]any{"integrations": []any{}})
			return
		}
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant",
			"no tenant context; pass X-Tenant-ID or X-Tenant-Slug", nil)
		return
	}
	rows, err := h.db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(tenantID),
			providersetting.IsActiveEQ(true),
		).
		All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	type summary struct {
		Provider  string            `json:"provider"`
		Name      string            `json:"name"`
		Connected bool              `json:"connected"`
		Fields    map[string]string `json:"fields"`
	}
	grouped := map[string]*summary{}
	for _, s := range rows {
		key := s.ProviderType + ":" + s.ProviderName
		g, ok := grouped[key]
		if !ok {
			g = &summary{Provider: s.ProviderType, Name: s.ProviderName, Fields: map[string]string{}, Connected: true}
			grouped[key] = g
		}
		if !s.IsSecret && !s.IsEncrypted {
			g.Fields[s.Key] = s.Value
		}
	}
	out := make([]*summary, 0, len(grouped))
	for _, v := range grouped {
		out = append(out, v)
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"integrations": out})
}

// AvailableIntegrations returns the list of platform-level provider types that
// have active configuration. The frontend uses this to show only integration
// cards whose OAuth apps / credentials are actually set up.
// @Summary List available platform integrations
// @Tags Integrations
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /integrations/available [get]
func (h *Handler) AvailableIntegrations(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ("platform"),
			providersetting.IsActiveEQ(true),
		).
		All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	seen := map[string]bool{}
	for _, s := range rows {
		seen[s.ProviderType] = true
	}
	types := make([]string, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"available": types})
}

// DisconnectTenant removes every ProviderSetting row for a given provider type.
// @Summary Disconnect a tenant integration
// @Tags Integrations
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param provider path string true "Provider type (google_calendar | meta_ads)"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /integrations/{provider} [delete]
func (h *Handler) DisconnectTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := h.resolveTenantID(r)
	if tenantID == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant",
			"no tenant context; pass X-Tenant-ID or X-Tenant-Slug", nil)
		return
	}
	provider := chirouter.URLParam(r, "provider")
	if provider == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_provider", "provider path param required", nil)
		return
	}
	n, err := h.db.ProviderSetting.Update().
		Where(
			providersetting.TenantIDEQ(tenantID),
			providersetting.ProviderTypeEQ(provider),
		).
		SetIsActive(false).
		SetStatus("disabled").
		Save(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "disconnect_failed", err.Error(), nil)
		return
	}
	revoked, err := h.revokeAccounts(r.Context(), tenantID, provider)
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "disconnect_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"disconnected": provider, "rows": n, "accounts_revoked": revoked})
}

type businessHoursInput struct {
	StartHour   *int   `json:"start_hour"`
	EndHour     *int   `json:"end_hour"`
	SlotMinutes *int   `json:"slot_minutes"`
	Days        string `json:"days"` // comma-separated weekday numbers, 0=Sunday..6=Saturday
}

// UpdateGoogleCalendarBusinessHours overrides the public booking widget's
// bookable hours for this tenant. Unset fields keep their existing value;
// omitted entirely, the widget falls back to gcal.DefaultBusinessHours().
// @Summary Configure booking widget business hours
// @Tags Integrations
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body businessHoursInput true "Business hours"
// @Success 200 {object} map[string]string
// @Failure 400 {object} httpapi.ErrorResponse
// @Router /integrations/google/business-hours [put]
func (h *Handler) UpdateGoogleCalendarBusinessHours(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := h.resolveTenantID(r)
	if tenantID == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant",
			"no tenant context; pass X-Tenant-ID or X-Tenant-Slug", nil)
		return
	}
	var in businessHoursInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid JSON body", nil)
		return
	}

	kv := map[string]string{}
	if in.StartHour != nil {
		if *in.StartHour < 0 || *in.StartHour > 23 {
			httpapi.Error(w, http.StatusBadRequest, "invalid_start_hour", "start_hour must be 0-23", nil)
			return
		}
		kv["business_hours_start"] = fmt.Sprintf("%d", *in.StartHour)
	}
	if in.EndHour != nil {
		if *in.EndHour < 0 || *in.EndHour > 23 {
			httpapi.Error(w, http.StatusBadRequest, "invalid_end_hour", "end_hour must be 0-23", nil)
			return
		}
		kv["business_hours_end"] = fmt.Sprintf("%d", *in.EndHour)
	}
	if in.SlotMinutes != nil {
		if *in.SlotMinutes <= 0 {
			httpapi.Error(w, http.StatusBadRequest, "invalid_slot_minutes", "slot_minutes must be positive", nil)
			return
		}
		kv["slot_minutes"] = fmt.Sprintf("%d", *in.SlotMinutes)
	}
	if in.Days != "" {
		kv["business_days"] = in.Days
	}
	if len(kv) == 0 {
		httpapi.Error(w, http.StatusBadRequest, "no_fields", "at least one field is required", nil)
		return
	}

	if err := h.upsertTenantKV(r.Context(), tenantID, "google_calendar", "oauth", kv, nil); err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// ── helpers ─────────────────────────────────────────────────────────────────

func (h *Handler) loadPlatform(ctx context.Context, provType, provName string) (map[string]string, error) {
	rows, err := h.db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ("platform"),
			providersetting.ProviderTypeEQ(provType),
			providersetting.ProviderNameEQ(provName),
			providersetting.IsActiveEQ(true),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		// client_secret / app_secret are encrypted at rest; SettingValue decrypts flagged rows
		// and re-seals any legacy plaintext secret.
		v, err := h.keys.SettingValue(ctx, h.db, r)
		if err != nil {
			return nil, fmt.Errorf("decrypt platform %s:%s %s: %w", provType, provName, r.Key, err)
		}
		out[r.Key] = v
	}
	if out["client_id"] == "" && out["app_id"] == "" {
		return nil, fmt.Errorf("platform OAuth client for %s:%s not configured", provType, provName)
	}
	return out, nil
}

func (h *Handler) upsertTenantKV(ctx context.Context, tenantID, provType, provName string, kv map[string]string, secretKeys []string) error {
	isSecret := func(k string) bool {
		for _, s := range secretKeys {
			if s == k {
				return true
			}
		}
		return false
	}
	for k, raw := range kv {
		// Tokens are encrypted at rest; with no key configured they are stored as-is and
		// re-sealed on first read once a key exists.
		v, encrypted := h.keys.SealSecret(ctx, k, raw, isSecret(k))
		existing, err := h.db.ProviderSetting.Query().
			Where(
				providersetting.TenantIDEQ(tenantID),
				providersetting.ProviderTypeEQ(provType),
				providersetting.ProviderNameEQ(provName),
				providersetting.KeyEQ(k),
			).
			Only(ctx)
		switch {
		case ent.IsNotFound(err):
			if _, err := h.db.ProviderSetting.Create().
				SetTenantID(tenantID).
				SetProviderType(provType).
				SetProviderName(provName).
				SetEnvironment("production").
				SetKey(k).
				SetValue(v).
				SetIsSecret(isSecret(k) || encryption.IsSecret(k)).
				SetIsEncrypted(encrypted).
				SetIsActive(true).
				SetStatus("active").
				Save(ctx); err != nil {
				return fmt.Errorf("create %s: %w", k, err)
			}
		case err != nil:
			return err
		default:
			if _, err := h.db.ProviderSetting.UpdateOne(existing).
				SetValue(v).
				SetIsSecret(isSecret(k) || encryption.IsSecret(k)).
				SetIsEncrypted(encrypted).
				SetIsActive(true).
				SetStatus("active").
				Save(ctx); err != nil {
				return fmt.Errorf("update %s: %w", k, err)
			}
		}
	}
	return nil
}

// integrationsURL is the UI page a connect callback returns to. UI routes are keyed by the
// tenant slug (/[orgSlug]/settings/integrations), so the tenant UUID from the OAuth state is
// mapped to its slug; the UUID is kept only as a last resort when the lookup fails (G12).
func (h *Handler) integrationsURL(ctx context.Context, tenantID, query string) string {
	org := tenantID
	if tid, err := uuid.Parse(tenantID); err == nil {
		if t, err := h.db.Tenant.Query().Where(tenant.IDEQ(tid)).Select(tenant.FieldSlug).Only(ctx); err == nil && t.Slug != "" {
			org = t.Slug
		}
	}
	return "/" + url.PathEscape(org) + "/settings/integrations?" + query
}

// signState returns `tenantID.userID.nonce.hmac` for a 10-minute validity window. userID is the
// signed-in auth user ("" for an API key caller); the callback records it as connected_by.
func (h *Handler) signState(tenantID, userID string) string {
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	payload := tenantID + "." + userID + "." + nonce
	mac := hmac.New(sha256.New, h.stateKey)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyState checks the signature and age and returns the tenant and user. The older
// `tenantID.nonce.hmac` form (no user) is still accepted.
func (h *Handler) verifyState(state string) (string, string, bool) {
	parts := strings.Split(state, ".")
	var tenantID, userID, nonce, sig string
	switch len(parts) {
	case 3:
		tenantID, nonce, sig = parts[0], parts[1], parts[2]
	case 4:
		tenantID, userID, nonce, sig = parts[0], parts[1], parts[2], parts[3]
	default:
		return "", "", false
	}
	mac := hmac.New(sha256.New, h.stateKey)
	mac.Write([]byte(strings.Join(parts[:len(parts)-1], ".")))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", "", false
	}
	// Age check (10 min). The nonce is signed, so a missing timestamp means a forged format.
	ns := parseInt64(nonce)
	if ns <= 0 || time.Since(time.Unix(0, ns)) > 10*time.Minute {
		return "", "", false
	}
	return tenantID, userID, true
}

func parseInt64(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
