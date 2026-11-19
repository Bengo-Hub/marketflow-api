// Package googleads serves a business's own Google Ads account in MarketFlow (owner 2026-11-19):
// the connected accounts, each account's campaign results, and the mapping from MarketFlow
// events (Lead, Purchase, Booking) to the account's own conversion actions that the conversions
// worker uploads to. The connection itself is integrations/googleads.go; tokens come from
// google.NewTokensFor, the same refresh path YouTube uses. Nothing here creates or spends.
package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
)

// AccountType is the ConnectedAccount type of a Google Ads account.
const AccountType = "google_ads_customer"

// MetaConversionActions in ConnectedAccount.metadata maps a MarketFlow event name to the
// account's conversion action resource (customers/{id}/conversionActions/{id}).
const MetaConversionActions = "conversion_actions"

// Events a business can map; the conversions worker sends these names.
var Events = []string{"Lead", "Booking", "Purchase"}

// Handler serves /api/v1/ads/google.
type Handler struct {
	DB     *ent.Client
	Keys   *encryption.KeyProvider
	Tokens *google.Tokens
	HTTP   *http.Client
	Base   string // Google Ads REST host; "" for Google's own
	Logger *zap.Logger
	// CanManage gates the mapping write (the approving roles); nil allows every member.
	CanManage func(ctx context.Context, tenantID uuid.UUID) bool
}

// New builds the handler with the platform Google Ads client's token refresh.
func New(db *ent.Client, keys *encryption.KeyProvider, logger *zap.Logger) *Handler {
	return &Handler{DB: db, Keys: keys, Tokens: google.NewTokensFor(db, keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName),
		HTTP: &http.Client{Timeout: 30 * time.Second}, Logger: logger}
}

func tenantOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}

// AccountView is one connected Google Ads account.
type AccountView struct {
	ConnectedID       string            `json:"connected_account_id"`
	CustomerID        string            `json:"customer_id"`
	Name              string            `json:"name"`
	Currency          string            `json:"currency,omitempty"`
	Status            string            `json:"status"`
	Test              bool              `json:"test_account,omitempty"`
	ConversionActions map[string]string `json:"conversion_actions"`
}

func viewOf(a *ent.ConnectedAccount) AccountView {
	v := AccountView{ConnectedID: a.ID.String(), CustomerID: a.ExternalID, Name: a.DisplayName, Status: a.Status,
		ConversionActions: MappingOf(a)}
	v.Currency, _ = a.Metadata["currency"].(string)
	v.Test, _ = a.Metadata["test_account"].(bool)
	return v
}

// MappingOf reads an account's event to conversion action mapping.
func MappingOf(a *ent.ConnectedAccount) map[string]string {
	out := map[string]string{}
	m, _ := a.Metadata[MetaConversionActions].(map[string]any)
	for k, v := range m {
		if s, ok := v.(string); ok && s != "" {
			out[k] = s
		}
	}
	return out
}

// Accounts handles GET /api/v1/ads/google/accounts: the tenant's connected Google Ads accounts.
// @Summary Connected Google Ads accounts
// @Tags Ads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/google/accounts [get]
func (h *Handler) Accounts(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	rows, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ(AccountType), connectedaccount.StatusNEQ("revoked")).
		Order(ent.Asc(connectedaccount.FieldDisplayName)).Limit(50).All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "could not read the Google Ads accounts", nil)
		return
	}
	out := make([]AccountView, 0, len(rows))
	for _, a := range rows {
		out = append(out, viewOf(a))
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out})
}

// account loads one of the tenant's active Google Ads accounts and a working caller for it.
func (h *Handler) account(w http.ResponseWriter, r *http.Request, tid uuid.UUID) (*ent.ConnectedAccount, google.Ads, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "account id required", nil)
		return nil, google.Ads{}, false
	}
	ctx := r.Context()
	a, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(id), connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ(AccountType)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			httpapi.Error(w, http.StatusNotFound, "not_found", "Google Ads account not found", nil)
			return nil, google.Ads{}, false
		}
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "could not read the account", nil)
		return nil, google.Ads{}, false
	}
	if a.Status != "active" {
		httpapi.Error(w, http.StatusConflict, "reconnect", "this Google Ads account needs connecting again", nil)
		return nil, google.Ads{}, false
	}
	_, extra, err := google.LoadClient(ctx, h.DB, h.Keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName)
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "oauth_not_configured", "the platform Google Ads client is not configured", nil)
		return nil, google.Ads{}, false
	}
	tok, err := h.Tokens.AccessToken(ctx, a)
	if err != nil {
		var te *google.TokenError
		if errors.As(err, &te) && te.Revoked() {
			httpapi.Error(w, http.StatusConflict, "reconnect", "Google ended this connection; connect the account again", nil)
			return nil, google.Ads{}, false
		}
		h.Logger.Warn("google ads token", zap.String("account", a.ID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "token_failed", "Google did not renew the access; try again shortly", nil)
		return nil, google.Ads{}, false
	}
	ads := google.AdsFromSettings(h.HTTP, extra, tok)
	ads.LoginCustomerID = "" // the account was connected directly, so it is read as itself
	if h.Base != "" {
		ads.Base = h.Base
	}
	return a, ads, true
}

func (h *Handler) adsFailed(w http.ResponseWriter, err error) {
	var ae *google.AdsError
	if errors.As(err, &ae) && ae.Status < 500 {
		httpapi.Error(w, http.StatusBadGateway, "google_ads_refused", ae.Message, nil)
		return
	}
	h.Logger.Warn("google ads call", zap.Error(err))
	httpapi.Error(w, http.StatusBadGateway, "google_ads_unavailable", "Google Ads did not answer; try again shortly", nil)
}

// Campaigns handles GET /api/v1/ads/google/accounts/{id}/campaigns?period=LAST_30_DAYS: each
// campaign's impressions, clicks, conversions and cost on the business's own account.
// @Summary Google Ads campaign results
// @Tags Ads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Connected account id"
// @Param period query string false "LAST_7_DAYS | LAST_14_DAYS | LAST_30_DAYS | THIS_MONTH | LAST_MONTH"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/google/accounts/{id}/campaigns [get]
func (h *Handler) Campaigns(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	a, ads, ok := h.account(w, r, tid)
	if !ok {
		return
	}
	rows, err := ads.CampaignResults(r.Context(), a.ExternalID, strings.ToUpper(r.URL.Query().Get("period")))
	if err != nil {
		h.adsFailed(w, err)
		return
	}
	cur, _ := a.Metadata["currency"].(string)
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": rows, "currency": cur})
}

// ConversionActions handles GET /api/v1/ads/google/accounts/{id}/conversion-actions: the
// account's enabled offline (upload click) conversion actions, for the mapping picker.
// @Summary Google Ads conversion actions
// @Tags Ads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Connected account id"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/google/accounts/{id}/conversion-actions [get]
func (h *Handler) ConversionActions(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	a, ads, ok := h.account(w, r, tid)
	if !ok {
		return
	}
	rows, err := ads.ConversionActions(r.Context(), a.ExternalID)
	if err != nil {
		h.adsFailed(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": rows, "events": Events, "mapping": MappingOf(a)})
}

// SetMapping handles PUT /api/v1/ads/google/accounts/{id}/conversion-actions {"Lead": "customers/1/conversionActions/2", ...}:
// which conversion action each MarketFlow event is sent to. An empty value removes the event.
// @Summary Map MarketFlow events to Google Ads conversion actions
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Connected account id"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/google/accounts/{id}/conversion-actions [put]
func (h *Handler) SetMapping(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	if h.CanManage != nil && !h.CanManage(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can change this", nil)
		return
	}
	var in map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "account id required", nil)
		return
	}
	ctx := r.Context()
	a, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(id), connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ(AccountType)).Only(ctx)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "Google Ads account not found", nil)
		return
	}
	mapping, bad := CleanMapping(a.ExternalID, in)
	if bad != "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", bad, nil)
		return
	}
	md := map[string]any{}
	for k, v := range a.Metadata {
		md[k] = v
	}
	md[MetaConversionActions] = mapping
	if err := h.DB.ConnectedAccount.UpdateOneID(a.ID).SetMetadata(md).Exec(ctx); err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the mapping", nil)
		return
	}
	a.Metadata = md
	httpapi.JSON(w, http.StatusOK, viewOf(a))
}

// CleanMapping keeps known events whose value is a conversion action of this same account.
func CleanMapping(customerID string, in map[string]string) (map[string]any, string) {
	known := map[string]bool{}
	for _, e := range Events {
		known[e] = true
	}
	prefix := "customers/" + google.CustomerDigits(customerID) + "/conversionActions/"
	out := map[string]any{}
	for k, v := range in {
		v = strings.TrimSpace(v)
		if !known[k] {
			return nil, "unknown event " + k + " (use " + strings.Join(Events, ", ") + ")"
		}
		if v == "" {
			continue
		}
		if !strings.HasPrefix(v, prefix) {
			return nil, "the conversion action for " + k + " must belong to this account"
		}
		out[k] = v
	}
	return out, ""
}
