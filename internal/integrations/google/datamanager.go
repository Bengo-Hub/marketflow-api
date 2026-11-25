package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Data Manager API: where offline conversions go since Google closed UploadClickConversions to new
// adopters on 2026-06-15 (CUSTOMER_NOT_ALLOWLISTED_FOR_THIS_FEATURE). The shapes below are from
// developers.google.com/data-manager/api (events.ingest, requestStatus.retrieve, the Google Ads
// offline send-events and upgrade pages, diagnostics and limits), read 2026-11-25. The same OAuth
// client is used; only the scope differs and the API must be enabled on its Cloud project.

const (
	// ScopeDataManager is the Data Manager API scope (sensitive).
	ScopeDataManager = "https://www.googleapis.com/auth/datamanager"
	// DataManagerBase is the Data Manager REST host and version.
	DataManagerBase = "https://datamanager.googleapis.com/v1"

	dmAccountType = "GOOGLE_ADS"
	// dmEventSource: the conversion is credited to a click on the business's website, which is what
	// an UPLOAD_CLICKS action ("Website, import from clicks") records. Google's own offline sample
	// sends WEB.
	dmEventSource = "WEB"
	dmAttempts    = 3
)

// GoogleAdsConnectScopes are asked for when a business connects Google Ads. Only ScopeAdWords is
// required: without ScopeDataManager results still show and conversions wait for a reconnect.
var GoogleAdsConnectScopes = []string{ScopeAdWords, ScopeDataManager}

// ErrReconnect means the connection lacks the permission to send conversions.
var ErrReconnect = errors.New(ReconnectMessage)

// ReconnectMessage is what the business reads when that permission is missing.
const ReconnectMessage = "Reconnect Google Ads and leave every permission ticked, so MarketFlow may send your conversions to Google."

var conversionActionRE = regexp.MustCompile(`^customers/(\d+)/conversionActions/(\d+)$`)

// ParseConversionAction splits customers/{customer}/conversionActions/{id}. Data Manager takes the
// numeric id, and the customer that owns the action as the operating account.
func ParseConversionAction(resource string) (customer, id string, ok bool) {
	m := conversionActionRE.FindStringSubmatch(strings.TrimSpace(resource))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// OfflineConversion is one conversion credited to a Google ad click.
type OfflineConversion struct {
	ConversionAction string // customers/{id}/conversionActions/{id}
	LoginCustomerID  string // the manager the connection signs in through, "" for direct access
	GCLID            string
	WBRAID           string
	GBRAID           string
	TransactionID    string // deduplicates: the same id again changes the first event, never adds one
	At               time.Time
	Value            float64
	Currency         string
}

// DataManager is a Data Manager API caller for one access token.
type DataManager struct {
	HTTP        *http.Client
	Base        string // DataManagerBase unless a test sets it
	AccessToken string
	// Sleep waits between attempts; nil waits for real.
	Sleep func(context.Context, time.Duration)
}

// DataManagerError is a Data Manager refusal (google.rpc.Status).
type DataManagerError struct {
	Status     int
	Code       string // PERMISSION_DENIED, INVALID_ARGUMENT, ...
	Reason     string // ErrorInfo reason or the first field violation's, e.g. INVALID_CONVERSION_ACTION_ID
	Field      string
	Message    string
	RequestID  string
	QuotaLimit string
}

func (e *DataManagerError) Error() string {
	s := fmt.Sprintf("data manager %d %s: %s", e.Status, e.Code, e.Message)
	if e.Reason != "" {
		s += " (" + e.Reason
		if e.Field != "" {
			s += " at " + e.Field
		}
		s += ")"
	}
	if e.RequestID != "" {
		s += " request " + e.RequestID
	}
	return s
}

// NeedsReconnect reports a token that is refused or lacks the Data Manager scope.
func (e *DataManagerError) NeedsReconnect() bool {
	return e.Status == http.StatusUnauthorized || e.Reason == "ACCESS_TOKEN_SCOPE_INSUFFICIENT"
}

func (e *DataManagerError) retryable() bool {
	if e.Status == http.StatusTooManyRequests {
		return !strings.Contains(e.QuotaLimit, "PerDay") // a daily quota comes back at midnight Pacific
	}
	return e.Status >= 500
}

func dataManagerError(status int, data []byte) *DataManagerError {
	var b struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Reason          string            `json:"reason"`
				Metadata        map[string]string `json:"metadata"`
				RequestID       string            `json:"requestId"`
				FieldViolations []struct {
					Field  string `json:"field"`
					Reason string `json:"reason"`
				} `json:"fieldViolations"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &b)
	e := &DataManagerError{Status: status, Code: b.Error.Status, Message: b.Error.Message}
	for _, d := range b.Error.Details {
		if e.Reason == "" {
			e.Reason = d.Reason
		}
		if d.RequestID != "" {
			e.RequestID = d.RequestID
		}
		if q := d.Metadata["quota_limit"]; q != "" {
			e.QuotaLimit = q
		}
	}
	// A field violation says more than the general INVALID_ARGUMENT.
	for _, d := range b.Error.Details {
		if len(d.FieldViolations) > 0 {
			if r := d.FieldViolations[0].Reason; r != "" {
				e.Reason = r
			}
			e.Field = d.FieldViolations[0].Field
			break
		}
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(data))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300]
		}
	}
	return e
}

func (d DataManager) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	base, hc := d.Base, d.HTTP
	if base == "" {
		base = DataManagerBase
	}
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.AccessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return dataManagerError(resp.StatusCode, data)
	}
	return json.Unmarshal(data, out)
}

func (d DataManager) sleep(ctx context.Context, w time.Duration) {
	if d.Sleep != nil {
		d.Sleep(ctx, w)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(w):
	}
}

// SendConversion sends one conversion (events:ingest) and returns Google's request id: Google
// processes it later, so RequestStatus says whether it counted. A busy or failing server is tried
// again up to dmAttempts times. warnings are optional fields Google ignored.
func (d DataManager) SendConversion(ctx context.Context, c OfflineConversion) (requestID string, warnings []string, err error) {
	customer, action, ok := ParseConversionAction(c.ConversionAction)
	if !ok {
		return "", nil, fmt.Errorf("%q is not a conversion action (customers/{id}/conversionActions/{id})", c.ConversionAction)
	}
	ids := map[string]any{}
	// One click id is enough. wbraid (web) goes before gbraid (app), as these are website leads.
	switch {
	case c.GCLID != "":
		ids["gclid"] = c.GCLID
	case c.WBRAID != "":
		ids["wbraid"] = c.WBRAID
	case c.GBRAID != "":
		ids["gbraid"] = c.GBRAID
	default:
		return "", nil, errors.New("the conversion has no Google click id")
	}
	dest := map[string]any{
		"operatingAccount":     map[string]any{"accountType": dmAccountType, "accountId": customer},
		"productDestinationId": action,
	}
	if login := CustomerDigits(c.LoginCustomerID); login != "" && login != customer {
		dest["loginAccount"] = map[string]any{"accountType": dmAccountType, "accountId": login}
	}
	// No consent field: MarketFlow records marketing consent, not the DMA ad user data and ad
	// personalisation choices Google means, and a value must never be made up.
	ev := map[string]any{
		"eventTimestamp": c.At.Format(time.RFC3339), "eventSource": dmEventSource, "adIdentifiers": ids,
	}
	// A lead or a booking has no value of its own: leaving it out lets the conversion's default value
	// in Google Ads apply, where an explicit 0 would not.
	if c.Value > 0 {
		ev["conversionValue"], ev["currency"] = c.Value, strings.ToUpper(c.Currency)
	}
	if c.TransactionID != "" {
		ev["transactionId"] = c.TransactionID
	}
	var out struct {
		RequestID     string `json:"requestId"`
		FieldWarnings []struct {
			Field       string `json:"field"`
			Description string `json:"description"`
			Reason      string `json:"reason"`
		} `json:"fieldWarnings"`
	}
	body := map[string]any{"destinations": []any{dest}, "events": []any{ev}}
	for i := 1; ; i++ {
		err = d.do(ctx, http.MethodPost, "/events:ingest", body, &out)
		if err == nil {
			break
		}
		// A lost answer is sent again too: the transaction id keeps it from counting twice.
		var de *DataManagerError
		if i == dmAttempts || ctx.Err() != nil || (errors.As(err, &de) && !de.retryable()) {
			return "", nil, err
		}
		d.sleep(ctx, time.Second<<(i-1))
	}
	for _, w := range out.FieldWarnings {
		warnings = append(warnings, strings.TrimSpace(w.Field+" "+w.Reason+" "+w.Description))
	}
	return out.RequestID, warnings, nil
}

// Request statuses (RequestStatus).
const (
	DMProcessing = "PROCESSING"
	DMSuccess    = "SUCCESS"
	DMFailed     = "FAILED"
	DMPartial    = "PARTIAL_SUCCESS"
)

// RequestStatus is how Google processed a request: its status and, when records failed, why.
type RequestStatus struct {
	Status   string
	Errors   []string // ProcessingErrorReason values
	Warnings []string // ProcessingWarningReason values
}

// RequestStatus reads how a sent request went (requestStatus:retrieve). MarketFlow sends one
// destination per request, so the first entry is the whole answer.
func (d DataManager) RequestStatus(ctx context.Context, requestID string) (RequestStatus, error) {
	type counts []struct {
		Reason string `json:"reason"`
	}
	var out struct {
		PerDestination []struct {
			RequestStatus string `json:"requestStatus"`
			ErrorInfo     struct {
				ErrorCounts counts `json:"errorCounts"`
			} `json:"errorInfo"`
			WarningInfo struct {
				WarningCounts counts `json:"warningCounts"`
			} `json:"warningInfo"`
		} `json:"requestStatusPerDestination"`
	}
	if err := d.do(ctx, http.MethodGet, "/requestStatus:retrieve?requestId="+url.QueryEscape(requestID), nil, &out); err != nil {
		return RequestStatus{}, err
	}
	if len(out.PerDestination) == 0 {
		return RequestStatus{Status: DMProcessing}, nil
	}
	x := out.PerDestination[0]
	st := RequestStatus{Status: x.RequestStatus}
	for _, c := range x.ErrorInfo.ErrorCounts {
		st.Errors = append(st.Errors, c.Reason)
	}
	for _, c := range x.WarningInfo.WarningCounts {
		st.Warnings = append(st.Warnings, c.Reason)
	}
	return st, nil
}

// uploadReasons explains the refusals a business or the platform owner can act on.
var uploadReasons = map[string]string{
	"SERVICE_DISABLED":                          "The Data Manager API is not switched on in MarketFlow's Google Cloud project. The platform owner enables it there.",
	"PERMISSION_DENIED":                         "The Google login used to connect may not send conversions for this account. Reconnect Google Ads with a login that has Standard or Admin access to it.",
	"INVALID_CONVERSION_ACTION_ID":              "The conversion chosen in MarketFlow no longer exists in Google Ads. Pick another one under Campaigns, Google Ads.",
	"INVALID_CONVERSION_ACTION_TYPE":            "The conversion chosen in MarketFlow is not of the type import from clicks. Pick another one under Campaigns, Google Ads.",
	"INVALID_DESTINATION":                       "Google did not accept the account or the conversion chosen in MarketFlow. Pick the conversion again under Campaigns, Google Ads.",
	"DESTINATION_ACCOUNT_TYPE_MISMATCH":         "The connected account is not a Google Ads account Google accepts conversions for.",
	"NOT_ALLOWLISTED":                           "Google has not opened this feature for the account.",
	"TERMS_AND_CONDITIONS_NOT_SIGNED":           "Accept Google's customer data terms in Google Ads, under Goals, Settings, then try again.",
	"EVENT_TIME_INVALID":                        "The sale happened outside the time Google accepts for a click.",
	"INVALID_CURRENCY_CODE":                     "Google does not know the sale's currency.",
	"CUSTOMER_NOT_ALLOWLISTED_FOR_THIS_FEATURE": "Google closed this way of sending conversions to new accounts. The platform owner sets google_ads.conversion_upload_api to data_manager in Service settings.",

	"PROCESSING_ERROR_REASON_EVENT_TOO_OLD":                                            "The sale is older than Google accepts.",
	"PROCESSING_ERROR_REASON_EXPIRED_CLICK":                                            "The ad click is older than the conversion's click window (90 days at most).",
	"PROCESSING_ERROR_REASON_TOO_RECENT_CLICK":                                         "The ad click was too recent for Google to match yet.",
	"PROCESSING_ERROR_REASON_CONVERSION_PRECEDES_CLICK":                                "The sale happened before the ad click.",
	"PROCESSING_ERROR_REASON_CLICK_NOT_FOUND":                                          "Google found no ad click that matches this lead.",
	"PROCESSING_ERROR_REASON_INVALID_CLICK":                                            "The click did not come from a Google ad Google can credit.",
	"PROCESSING_ERROR_REASON_INVALID_GCLID":                                            "Google could not read the click id.",
	"PROCESSING_ERROR_REASON_INVALID_GBRAID":                                           "Google could not read the click id.",
	"PROCESSING_ERROR_REASON_INVALID_WBRAID":                                           "Google could not read the click id.",
	"PROCESSING_ERROR_REASON_INVALID_AD_IDENTIFIERS":                                   "Google could not read the click id.",
	"PROCESSING_ERROR_REASON_DUPLICATE_GCLID":                                          "Google already has this conversion.",
	"PROCESSING_ERROR_REASON_DUPLICATE_TRANSACTION_ID":                                 "Google already has this conversion.",
	"PROCESSING_ERROR_REASON_DENIED_CONSENT":                                           "The person, or the Google Ads account's own default, does not allow the data to be used for ads.",
	"PROCESSING_ERROR_REASON_NO_CONSENT":                                               "Google Ads has no consent setting it can use. Check consent under Goals, Settings in Google Ads.",
	"PROCESSING_ERROR_REASON_UNKNOWN_CONSENT":                                          "Google Ads has no consent setting it can use. Check consent under Goals, Settings in Google Ads.",
	"PROCESSING_ERROR_REASON_DESTINATION_TOO_RECENTLY_CREATED":                         "The conversion was created too recently. Google accepts it after a few hours.",
	"PROCESSING_ERROR_REASON_INVALID_OPERATING_ACCOUNT_FOR_CLICK":                      "The click belongs to another Google Ads account.",
	"PROCESSING_ERROR_OPERATING_ACCOUNT_MISMATCH_FOR_AD_IDENTIFIER":                    "The click belongs to another Google Ads account.",
	"PROCESSING_ERROR_REASON_ONE_PER_CLICK_CONVERSION_ACTION_NOT_PERMITTED_WITH_BRAID": "Google takes clicks from iPhones (gbraid, wbraid) only on a conversion that counts every conversion. Change Count to Every on the conversion in Google Ads.",
}

// PlainReason explains a Data Manager or Google Ads reason code; an unknown one is shown in words.
func PlainReason(code string) string {
	if s := uploadReasons[code]; s != "" {
		return s
	}
	s := strings.TrimPrefix(strings.TrimPrefix(code, "PROCESSING_ERROR_REASON_"), "PROCESSING_ERROR_")
	return "Google's reason: " + strings.ToLower(strings.ReplaceAll(s, "_", " ")) + "."
}

// PlainUploadError explains a failed conversion upload for the business. reconnect reports that
// only connecting again helps.
func PlainUploadError(err error) (text string, reconnect bool) {
	var de *DataManagerError
	var te *TokenError
	var ae *AdsError
	switch {
	case errors.Is(err, ErrReconnect), errors.Is(err, ErrNoRefreshToken), errors.As(err, &te) && te.Revoked():
		return ReconnectMessage, true
	case errors.As(err, &de):
		if de.NeedsReconnect() {
			return ReconnectMessage, true
		}
		if s := uploadReasons[de.Reason]; s != "" {
			return s, false
		}
		if s := uploadReasons[de.Code]; s != "" {
			return s, false
		}
		if de.Status >= 500 || de.Status == http.StatusTooManyRequests {
			return "Google was too busy to take the conversion.", false
		}
		return "Google refused the conversion: " + de.Message, false
	case errors.As(err, &ae):
		// A partial failure carries the code only inside its message.
		code := "CUSTOMER_NOT_ALLOWLISTED_FOR_THIS_FEATURE"
		if ae.Code == code || strings.Contains(ae.Message, code) {
			return uploadReasons[code], false
		}
		return "Google refused the conversion: " + ae.Message, false
	}
	return "Google did not answer, so the conversion was not sent.", false
}
