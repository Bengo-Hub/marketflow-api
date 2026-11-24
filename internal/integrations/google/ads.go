package google

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Google Ads API calls MarketFlow makes for a business's own Google Ads account (owner
// 2026-11-19: connect, results, conversion upload; scope https://www.googleapis.com/auth/adwords).
// The platform client is ProviderSetting "google_ads"/"ads": client_id, client_secret,
// redirect_url, login_customer_id (manager account, optional) and api_version (optional,
// AdsAPIVersion when empty). REST, not the gRPC client library, to keep the binary small.
//
// No developer token. Google retired them on 2026-09-09: the API ignores the header today and
// a future major version rejects it, so MarketFlow never sends one, and a developer_token
// still stored on the row is ignored. API access now belongs to the Google Cloud project that
// owns the OAuth client (marketflow-os); its level (Test, Explorer, Basic, Standard) is set on
// that project's Google Ads API Overview page (developers.google.com/google-ads/api/docs/api-policy/developer-token).

const (
	// ProviderGoogleAds and ProviderGoogleAdsName are the platform client's ProviderSetting row.
	ProviderGoogleAds     = "google_ads"
	ProviderGoogleAdsName = "ads"
	// ScopeAdWords is the one Google Ads scope.
	ScopeAdWords = "https://www.googleapis.com/auth/adwords"
	// AdsAPIBase is the Google Ads REST host.
	AdsAPIBase = "https://googleads.googleapis.com"
	// AdsAPIVersion is the version called when the provider row sets none. Google sunsets each
	// major version about 12 months after release; v26 (2026-10-14) is supported until November
	// 2027 (developers.google.com/google-ads/api/docs/sunset-dates). Set api_version in Platform >
	// Providers to move forward without a release.
	AdsAPIVersion = "v26"
)

// Ads is a Google Ads API caller for one access token.
type Ads struct {
	HTTP    *http.Client
	Base    string // AdsAPIBase unless a test sets it
	Version string
	// LoginCustomerID is the manager account the call goes through, when the customer is reached
	// through one (digits only).
	LoginCustomerID string
	AccessToken     string
}

// AdsFromSettings builds a caller from the platform row's extra keys.
func AdsFromSettings(hc *http.Client, extra map[string]string, accessToken string) Ads {
	v := strings.TrimSpace(extra["api_version"])
	if v == "" {
		v = AdsAPIVersion
	}
	return Ads{HTTP: hc, Base: AdsAPIBase, Version: v,
		LoginCustomerID: CustomerDigits(extra["login_customer_id"]), AccessToken: accessToken}
}

// CustomerDigits strips the dashes from a customer id ("932-084-9278" to "9320849278").
func CustomerDigits(id string) string { return strings.ReplaceAll(strings.TrimSpace(id), "-", "") }

// AdsError is a Google Ads API refusal.
type AdsError struct {
	Status  int
	Message string
	// Code is the first Google Ads error code in the details, e.g. "CUSTOMER_NOT_ENABLED". Google's
	// top-level message for a 403 is only "The caller does not have permission"; the code says why.
	Code string
}

func (e *AdsError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("google ads %d: %s (%s)", e.Status, e.Message, e.Code)
	}
	return fmt.Sprintf("google ads %d: %s", e.Status, e.Message)
}

// ErrCustomerNotEnabled is Google's code for an account that is not set up yet (billing or the
// first campaign unfinished) or was cancelled.
const ErrCustomerNotEnabled = "CUSTOMER_NOT_ENABLED"

// adsErrorCode returns the first error code in a Google Ads failure body
// (error.details[].errors[].errorCode, an object with one key such as authorizationError).
func adsErrorCode(data []byte) string {
	var e struct {
		Error struct {
			Details []struct {
				Errors []struct {
					ErrorCode map[string]any `json:"errorCode"`
				} `json:"errors"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return ""
	}
	for _, d := range e.Error.Details {
		for _, x := range d.Errors {
			for _, v := range x.ErrorCode {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// errProjectNotApproved is what Google returns (v25 on) when the Cloud project only has Test
// access and the call reaches a real account.
const errProjectNotApproved = "CLOUD_PROJECT_NOT_APPROVED_FOR_PRODUCTION"

// ProjectAccessMessage replaces Google's raw refusal with what to do about it.
const ProjectAccessMessage = "MarketFlow's Google Cloud project only has Test access to the Google Ads API. " +
	"The platform owner requests Explorer or Basic access on the project's Google Ads API Overview page in the Cloud Console."

func (a Ads) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	base := a.Base
	if base == "" {
		base = AdsAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/"+a.Version+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.LoginCustomerID != "" {
		req.Header.Set("login-customer-id", a.LoginCustomerID)
	}
	hc := a.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error.Message
		if strings.Contains(string(data), errProjectNotApproved) {
			msg = ProjectAccessMessage
		}
		if msg == "" {
			msg = strings.TrimSpace(string(data))
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		return &AdsError{Status: resp.StatusCode, Message: msg, Code: adsErrorCode(data)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// AccessibleCustomers lists the customer ids the signed-in Google identity can reach directly.
func (a Ads) AccessibleCustomers(ctx context.Context) ([]string, error) {
	var out struct {
		ResourceNames []string `json:"resourceNames"`
	}
	if err := a.do(ctx, http.MethodGet, "/customers:listAccessibleCustomers", nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.ResourceNames))
	for _, rn := range out.ResourceNames {
		ids = append(ids, strings.TrimPrefix(rn, "customers/"))
	}
	return ids, nil
}

// Customer is one Google Ads account.
type Customer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	TimeZone string `json:"time_zone"`
	Manager  bool   `json:"manager"`
	Test     bool   `json:"test"`
}

// search runs one GAQL query on a customer (googleAds:search, first page of up to 10,000 rows).
func (a Ads) search(ctx context.Context, customerID, query string) ([]map[string]any, error) {
	var out struct {
		Results []map[string]any `json:"results"`
	}
	err := a.do(ctx, http.MethodPost, "/customers/"+CustomerDigits(customerID)+"/googleAds:search", map[string]any{"query": query}, &out)
	return out.Results, err
}

// CustomerInfo reads a customer's name, currency, time zone and kind.
func (a Ads) CustomerInfo(ctx context.Context, customerID string) (Customer, error) {
	rows, err := a.search(ctx, customerID,
		"SELECT customer.id, customer.descriptive_name, customer.currency_code, customer.time_zone, customer.manager, customer.test_account FROM customer LIMIT 1")
	if err != nil {
		return Customer{}, err
	}
	c := Customer{ID: CustomerDigits(customerID)}
	if len(rows) == 0 {
		return c, nil
	}
	m, _ := rows[0]["customer"].(map[string]any)
	c.Name, _ = m["descriptiveName"].(string)
	c.Currency, _ = m["currencyCode"].(string)
	c.TimeZone, _ = m["timeZone"].(string)
	c.Manager, _ = m["manager"].(bool)
	c.Test, _ = m["testAccount"].(bool)
	return c, nil
}

// CampaignResult is one campaign's results over the period.
type CampaignResult struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Channel     string  `json:"channel"`
	Impressions int64   `json:"impressions"`
	Clicks      int64   `json:"clicks"`
	Conversions float64 `json:"conversions"`
	// Cost is in the account currency (cost_micros / 1,000,000).
	Cost float64 `json:"cost"`
}

// periods Google Ads accepts in DURING.
var periods = map[string]bool{"LAST_7_DAYS": true, "LAST_14_DAYS": true, "LAST_30_DAYS": true, "THIS_MONTH": true, "LAST_MONTH": true}

// CampaignResults reads every non-removed campaign's results for the period (default LAST_30_DAYS).
func (a Ads) CampaignResults(ctx context.Context, customerID, period string) ([]CampaignResult, error) {
	if !periods[period] {
		period = "LAST_30_DAYS"
	}
	rows, err := a.search(ctx, customerID, "SELECT campaign.id, campaign.name, campaign.status, campaign.advertising_channel_type, "+
		"metrics.impressions, metrics.clicks, metrics.conversions, metrics.cost_micros FROM campaign "+
		"WHERE campaign.status != 'REMOVED' AND segments.date DURING "+period+" ORDER BY metrics.cost_micros DESC LIMIT 200")
	if err != nil {
		return nil, err
	}
	out := make([]CampaignResult, 0, len(rows))
	for _, r := range rows {
		c, _ := r["campaign"].(map[string]any)
		m, _ := r["metrics"].(map[string]any)
		res := CampaignResult{}
		res.ID = fmt.Sprint(c["id"])
		res.Name, _ = c["name"].(string)
		res.Status, _ = c["status"].(string)
		res.Channel, _ = c["advertisingChannelType"].(string)
		res.Impressions = int64Of(m["impressions"])
		res.Clicks = int64Of(m["clicks"])
		res.Conversions, _ = m["conversions"].(float64)
		res.Cost = float64(int64Of(m["costMicros"])) / 1e6
		out = append(out, res)
	}
	return out, nil
}

// ConversionAction is one of the account's conversion actions a MarketFlow event can map to.
type ConversionAction struct {
	ResourceName string `json:"resource_name"`
	Name         string `json:"name"`
	Status       string `json:"status"`
}

// ConversionActions lists the account's enabled upload-click conversion actions.
func (a Ads) ConversionActions(ctx context.Context, customerID string) ([]ConversionAction, error) {
	rows, err := a.search(ctx, customerID, "SELECT conversion_action.resource_name, conversion_action.name, conversion_action.status "+
		"FROM conversion_action WHERE conversion_action.type = 'UPLOAD_CLICKS' AND conversion_action.status = 'ENABLED' LIMIT 100")
	if err != nil {
		return nil, err
	}
	out := make([]ConversionAction, 0, len(rows))
	for _, r := range rows {
		c, _ := r["conversionAction"].(map[string]any)
		ca := ConversionAction{}
		ca.ResourceName, _ = c["resourceName"].(string)
		ca.Name, _ = c["name"].(string)
		ca.Status, _ = c["status"].(string)
		out = append(out, ca)
	}
	return out, nil
}

// ClickConversion is one offline conversion to upload.
type ClickConversion struct {
	GCLID            string
	ConversionAction string // customers/{id}/conversionActions/{id}
	DateTime         string // "2026-04-16 09:30:00+03:00"
	Value            float64
	Currency         string
}

// UploadClickConversions uploads conversions with partial failure on, so one bad row does not
// drop the others; a partial failure comes back as an error naming it.
func (a Ads) UploadClickConversions(ctx context.Context, customerID string, convs []ClickConversion) error {
	rows := make([]map[string]any, 0, len(convs))
	for _, c := range convs {
		rows = append(rows, map[string]any{"gclid": c.GCLID, "conversionAction": c.ConversionAction,
			"conversionDateTime": c.DateTime, "conversionValue": c.Value, "currencyCode": c.Currency})
	}
	var out struct {
		PartialFailureError *struct {
			Message string `json:"message"`
		} `json:"partialFailureError"`
	}
	if err := a.do(ctx, http.MethodPost, "/customers/"+CustomerDigits(customerID)+":uploadClickConversions",
		map[string]any{"conversions": rows, "partialFailure": true}, &out); err != nil {
		return err
	}
	if out.PartialFailureError != nil && out.PartialFailureError.Message != "" {
		return &AdsError{Status: http.StatusOK, Message: "partial failure: " + out.PartialFailureError.Message}
	}
	return nil
}

// int64Of reads a number Google Ads sends as a JSON string ("1234") or a number.
func int64Of(v any) int64 {
	switch n := v.(type) {
	case string:
		var x int64
		_, _ = fmt.Sscan(n, &x)
		return x
	case float64:
		return int64(n)
	}
	return 0
}
