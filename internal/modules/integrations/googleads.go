package integrations

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
)

// Google Ads account connection (owner 2026-11-19, replaces "coming soon"). The business signs in
// with the Google identity that can reach its Google Ads account; MarketFlow asks only for the
// adwords scope, lists the accounts that identity reaches, and keeps one ConnectedAccount row per
// account with the refresh token as ciphertext (the same store and refresh path as YouTube:
// google.NewTokensFor). What it is used for: the account's campaign results in MarketFlow, and
// offline conversions (a lead that booked or bought) sent back to the business's own conversion
// actions (worker/conversions.go). Nothing is created or spent on Google Ads from MarketFlow.

// providerGoogleAds is the provider type the integrations page and the disconnect route use.
const providerGoogleAds = google.ProviderGoogleAds

// AccountGoogleAdsCustomer is a business's Google Ads account.
const AccountGoogleAdsCustomer = "google_ads_customer"

// maxGoogleAdsAccounts caps how many accounts one connect stores (an agency identity can reach
// hundreds; a small business has one or two).
const maxGoogleAdsAccounts = 20

// googleAdsAccounts turns the reachable, non-manager accounts into ConnectedAccount rows.
func googleAdsAccounts(custs []google.Customer, tok google.Token, now time.Time) []account {
	out := make([]account, 0, len(custs))
	for _, c := range custs {
		if c.Manager {
			continue // a manager account runs no ads itself; its client accounts are listed instead
		}
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = "Google Ads " + c.ID
		}
		out = append(out, account{
			Platform: PlatformGoogle, Type: AccountGoogleAdsCustomer, ExternalID: c.ID, DisplayName: name,
			Token: tok.Access, Refresh: tok.Refresh, Scopes: tok.Scopes,
			Metadata: map[string]any{
				google.MetaAccessExpiresAt: now.Add(tok.ExpiresIn).UTC().Format(time.RFC3339),
				"currency":                 c.Currency, "time_zone": c.TimeZone, "test_account": c.Test,
			},
		})
	}
	return out
}

// GoogleAdsAuthorize returns the Google consent URL for connecting a Google Ads account.
// @Summary Start the Google Ads account connection for a tenant
// @Tags Integrations
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]string
// @Failure 503 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /integrations/google-ads/authorize [get]
func (h *Handler) GoogleAdsAuthorize(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := h.resolveTenantID(r)
	if tenantID == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "no tenant context; pass X-Tenant-ID or X-Tenant-Slug", nil)
		return
	}
	// No developer token check: Google retired them on 2026-09-09 (see google/ads.go).
	c, _, err := google.LoadClient(r.Context(), h.db, h.keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName)
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "oauth_not_configured", "platform Google Ads client not configured", nil)
		return
	}
	state, ok := h.issueStateOrFail(w, r, tenantID)
	if !ok {
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]string{"url": google.AuthCodeURL(c, []string{google.ScopeAdWords}, state)})
}

// GoogleAdsCallback exchanges the code, lists the reachable accounts and stores them.
// @Summary Google Ads OAuth callback
// @Tags Integrations
// @Produce html
// @Param code query string true "Authorization code"
// @Param state query string true "Signed state"
// @Router /integrations/google-ads/callback [get]
func (h *Handler) GoogleAdsCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID, userID, stErr := h.redeemState(w, r, r.URL.Query().Get("state"))
	if stErr != nil {
		h.failConnect(w, r, "google_ads", tenantID, stateReason(stErr), stErr.Error())
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		h.failConnect(w, r, "google_ads", tenantID, "cancelled", r.URL.Query().Get("error"))
		return
	}
	if h.keys == nil || !h.keys.Configured(ctx) {
		h.failConnect(w, r, "google_ads", tenantID, "oauth_not_configured", "no encryption key configured")
		return
	}
	c, extra, err := google.LoadClient(ctx, h.db, h.keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName)
	if err != nil {
		h.failConnect(w, r, "google_ads", tenantID, "oauth_not_configured", err.Error())
		return
	}
	tok, err := h.googleOAuth.Exchange(ctx, c, code)
	if err != nil {
		h.failConnect(w, r, "google_ads", tenantID, "token_exchange_failed", err.Error())
		return
	}
	if tok.Refresh == "" {
		h.failConnect(w, r, "google_ads", tenantID, "no_refresh_token", "Google returned no refresh_token")
		return
	}
	if !google.HasScopes(tok.Scopes, []string{google.ScopeAdWords}) {
		h.revokeGrant(ctx, tenantID, tok.Refresh)
		h.failConnect(w, r, "google_ads", tenantID, "missing_scope", "granted: "+strings.Join(tok.Scopes, " "))
		return
	}
	custs, err := h.googleAdsCustomers(ctx, extra, tok.Access)
	if err != nil {
		h.revokeGrant(ctx, tenantID, tok.Refresh)
		h.failConnect(w, r, "google_ads", tenantID, "account_read_failed", err.Error())
		return
	}
	accs := googleAdsAccounts(custs, tok, time.Now())
	if len(accs) == 0 {
		h.revokeGrant(ctx, tenantID, tok.Refresh)
		h.failConnect(w, r, "google_ads", tenantID, "no_account", "this Google identity reaches no Google Ads account that runs ads")
		return
	}
	if err := h.saveAccounts(ctx, tenantID, userID, accs); err != nil {
		h.failConnect(w, r, "google_ads", tenantID, "persist_failed", err.Error())
		return
	}
	target := h.integrationsURL(ctx, tenantID, "google_ads=connected&account="+url.QueryEscape(accs[0].DisplayName))
	http.Redirect(w, r, target, http.StatusFound)
}

// googleAdsCustomers lists the accounts the token reaches with their names. The direct list call
// needs no login customer; each account is then read as itself.
func (h *Handler) googleAdsCustomers(ctx context.Context, extra map[string]string, access string) ([]google.Customer, error) {
	ads := google.AdsFromSettings(h.httpClient(), extra, access)
	ads.LoginCustomerID = "" // a direct read of each reachable account needs no manager header
	if h.googleAdsBase != "" {
		ads.Base = h.googleAdsBase
	}
	ids, err := ads.AccessibleCustomers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]google.Customer, 0, len(ids))
	for i, id := range ids {
		if i == maxGoogleAdsAccounts {
			break
		}
		cust, err := ads.CustomerInfo(ctx, id)
		if err != nil {
			// An account the identity lists but cannot read (cancelled, no access) is skipped.
			h.logger.Info("google ads account not readable", zap.String("customer", id), zap.Error(err))
			continue
		}
		out = append(out, cust)
	}
	return out, nil
}
