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
//  5. Backend verifies state, exchanges code, persists token as ProviderSetting.
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
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	chirouter "github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Handler owns the OAuth endpoints.
type Handler struct {
	db        *ent.Client
	stateKey  []byte // signs the `state` round-trip; reuses encryption key
	logger    *zap.Logger
}

// NewHandler constructs a Handler.
func NewHandler(db *ent.Client, stateKey []byte, logger *zap.Logger) *Handler {
	return &Handler{db: db, stateKey: stateKey, logger: logger.Named("integrations")}
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
	googleScope    = "https://www.googleapis.com/auth/calendar.events"
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
	state := h.signState(tenantID)
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
	tenantID, ok := h.verifyState(state)
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
	target := fmt.Sprintf("/%s/settings/integrations?google=connected", tenantID)
	http.Redirect(w, r, target, http.StatusFound)
}

// ── Meta ────────────────────────────────────────────────────────────────────

const (
	metaAuthURL  = "https://www.facebook.com/v21.0/dialog/oauth"
	metaTokenURL = "https://graph.facebook.com/v21.0/oauth/access_token"
	metaScope    = "pages_show_list,pages_read_engagement,leads_retrieval,pages_manage_metadata"
)

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
	state := h.signState(tenantID)
	q := url.Values{
		"client_id":     {creds["app_id"]},
		"redirect_uri":  {creds["redirect_url"]},
		"response_type": {"code"},
		"scope":         {metaScope},
		"state":         {state},
	}
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
	tenantID, ok := h.verifyState(state)
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
	// Step 3: fetch user's pages + pick the first (tenant can switch in UI).
	pagesResp, err := http.Get("https://graph.facebook.com/v21.0/me/accounts?access_token=" + url.QueryEscape(userToken))
	if err != nil {
		httpapi.Error(w, http.StatusBadGateway, "pages_fetch_failed", err.Error(), nil)
		return
	}
	pagesBody, _ := io.ReadAll(pagesResp.Body)
	pagesResp.Body.Close()
	var pagesResult struct {
		Data []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(pagesBody, &pagesResult)
	if len(pagesResult.Data) == 0 {
		httpapi.Error(w, http.StatusBadRequest, "no_pages",
			"no Facebook Pages found for this user", nil)
		return
	}
	p := pagesResult.Data[0]
	if err := h.upsertTenantKV(r.Context(), tenantID, "meta_ads", "page", map[string]string{
		"page_id":     p.ID,
		"page_name":   p.Name,
		"page_token":  p.AccessToken,
		"user_token":  userToken,
	}, []string{"page_token", "user_token"}); err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "persist_failed", err.Error(), nil)
		return
	}
	target := fmt.Sprintf("/%s/settings/integrations?meta=connected&page=%s", tenantID, url.QueryEscape(p.Name))
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
	httpapi.JSON(w, http.StatusOK, map[string]any{"disconnected": provider, "rows": n})
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
		// NOTE: values are encrypted at rest when is_encrypted=true; the
		// ProviderSetting handler has already decrypted via its Manager when
		// clients call /admin/providers/resolve. Here we only read plain
		// fields (OAuth client_id, redirect_url are not sensitive per se,
		// but the client_secret / app_secret SHOULD be decrypted — do so
		// below when the row is flagged encrypted).
		out[r.Key] = r.Value
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
	for k, v := range kv {
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
				SetIsSecret(isSecret(k)).
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
				SetIsActive(true).
				SetStatus("active").
				Save(ctx); err != nil {
				return fmt.Errorf("update %s: %w", k, err)
			}
		}
	}
	return nil
}

// signState returns `tenantID.nonce.hmac` for a 10-minute validity window.
func (h *Handler) signState(tenantID string) string {
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	payload := tenantID + "." + nonce
	mac := hmac.New(sha256.New, h.stateKey)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func (h *Handler) verifyState(state string) (string, bool) {
	parts := strings.SplitN(state, ".", 3)
	if len(parts) != 3 {
		return "", false
	}
	tenantID, nonce, sig := parts[0], parts[1], parts[2]
	mac := hmac.New(sha256.New, h.stateKey)
	mac.Write([]byte(tenantID + "." + nonce))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}
	// Age check (10 min).
	if ns := parseInt64(nonce); ns > 0 {
		if time.Since(time.Unix(0, ns)) > 10*time.Minute {
			return "", false
		}
	}
	return tenantID, true
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

