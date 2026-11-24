package google

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func dmServer(t *testing.T, answers ...func(http.ResponseWriter)) (DataManager, *[]map[string]any) {
	t.Helper()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/events:ingest" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		got = append(got, m)
		i := len(got) - 1
		if i >= len(answers) {
			i = len(answers) - 1
		}
		answers[i](w)
	}))
	t.Cleanup(srv.Close)
	return DataManager{HTTP: srv.Client(), Base: srv.URL, AccessToken: "tok", Sleep: func(context.Context, time.Duration) {}}, &got
}

func answer(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(status); _, _ = io.WriteString(w, body) }
}

func conv() OfflineConversion {
	return OfflineConversion{ConversionAction: "customers/2018958413/conversionActions/77", GCLID: "G1", WBRAID: "W1", TransactionID: "tx-1",
		At: time.Date(2026, 11, 25, 12, 30, 0, 0, time.FixedZone("EAT", 3*3600)), Value: 2500.5, Currency: "kes"}
}

func TestSendConversionRequest(t *testing.T) {
	d, got := dmServer(t, answer(200, `{"requestId":"req-1","fieldWarnings":[{"field":"events.events[0].currency","reason":"R","description":"D"}]}`))
	id, warns, err := d.SendConversion(context.Background(), conv())
	if err != nil || id != "req-1" || len(warns) != 1 {
		t.Fatalf("%q %v %v", id, warns, err)
	}
	b, _ := json.Marshal((*got)[0])
	want := `{"destinations":[{"operatingAccount":{"accountId":"2018958413","accountType":"GOOGLE_ADS"},"productDestinationId":"77"}],` +
		`"events":[{"adIdentifiers":{"gclid":"G1"},"conversionValue":2500.5,"currency":"KES","eventSource":"WEB","eventTimestamp":"2026-11-25T12:30:00+03:00","transactionId":"tx-1"}]}`
	if string(b) != want {
		t.Errorf("body\n%s\nwant\n%s", b, want)
	}
	// Without a gclid the web id is used; a manager becomes the login account; no consent is made up.
	c := conv()
	c.GCLID, c.GBRAID, c.LoginCustomerID = "", "B1", "932-084-9278"
	if _, _, err := d.SendConversion(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal((*got)[1])
	if s := string(b); !strings.Contains(s, `"adIdentifiers":{"wbraid":"W1"}`) || !strings.Contains(s, `"loginAccount":{"accountId":"9320849278"`) || strings.Contains(s, "consent") {
		t.Errorf("body %s", s)
	}
	c.WBRAID = ""
	if _, _, err := d.SendConversion(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if b, _ = json.Marshal((*got)[2]); !strings.Contains(string(b), `{"gbraid":"B1"}`) {
		t.Errorf("body %s", b)
	}
	c.GBRAID = ""
	if _, _, err := d.SendConversion(context.Background(), c); err == nil {
		t.Error("no click id is refused before the call")
	}
	c = conv()
	c.ConversionAction = "77"
	if _, _, err := d.SendConversion(context.Background(), c); err == nil || len(*got) != 3 {
		t.Error("a bare id is refused before the call")
	}
}

func TestSendConversionRetries(t *testing.T) {
	d, got := dmServer(t, answer(503, `{}`), answer(429, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`), answer(200, `{"requestId":"r"}`))
	if id, _, err := d.SendConversion(context.Background(), conv()); err != nil || id != "r" || len(*got) != 3 {
		t.Errorf("%q %v after %d", id, err, len(*got))
	}
	d, got = dmServer(t, answer(429, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"reason":"RATE_LIMIT_EXCEEDED","metadata":{"quota_limit":"IngestionMutateRequestsPerDayPerProject"}}]}}`))
	if _, _, err := d.SendConversion(context.Background(), conv()); err == nil || len(*got) != 1 {
		t.Errorf("a daily quota is not retried: %d calls", len(*got))
	}
}

func TestUploadErrorsInPlainWords(t *testing.T) {
	for _, c := range []struct {
		name, body string
		status     int
		want       string
		reconnect  bool
	}{
		{"missing scope", `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Request had insufficient authentication scopes.","details":[{"reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT","domain":"googleapis.com"}]}}`, 403, "Reconnect Google Ads", true},
		{"token refused", `{"error":{"status":"UNAUTHENTICATED"}}`, 401, "Reconnect Google Ads", true},
		{"api off", `{"error":{"status":"PERMISSION_DENIED","details":[{"reason":"SERVICE_DISABLED"}]}}`, 403, "not switched on", false},
		{"read only login", `{"error":{"status":"PERMISSION_DENIED","details":[{"reason":"PERMISSION_DENIED"}]}}`, 403, "Standard or Admin", false},
		{"field", `{"error":{"status":"INVALID_ARGUMENT","message":"There was a problem with the request.","details":[{"reason":"INVALID_ARGUMENT"},{"requestId":"t-9"},{"fieldViolations":[{"field":"destinations","reason":"INVALID_CONVERSION_ACTION_TYPE"}]}]}}`, 400, "import from clicks", false},
		{"unknown", `{"error":{"status":"INVALID_ARGUMENT","message":"Odd."}}`, 400, "Google refused the conversion: Odd.", false},
	} {
		d, got := dmServer(t, answer(c.status, c.body))
		_, _, err := d.SendConversion(context.Background(), conv())
		text, rec := PlainUploadError(err)
		if !strings.Contains(text, c.want) || rec != c.reconnect || len(*got) != 1 {
			t.Errorf("%s: %q %v (%v) after %d calls", c.name, text, rec, err, len(*got))
		}
	}
	if text, rec := PlainUploadError(&AdsError{Status: 200, Message: "partial failure: CUSTOMER_NOT_ALLOWLISTED_FOR_THIS_FEATURE"}); rec || !strings.Contains(text, "data_manager") {
		t.Errorf("allowlist: %q", text)
	}
	if _, rec := PlainUploadError(&TokenError{Code: "invalid_grant"}); !rec {
		t.Error("a revoked token needs a reconnect")
	}
	if text, rec := PlainUploadError(errors.New("dial tcp")); rec || strings.Contains(text, "dial") {
		t.Errorf("network: %q", text)
	}
	if s := PlainReason("PROCESSING_ERROR_REASON_SOMETHING_NEW"); s != "Google's reason: something new." {
		t.Errorf("unknown reason: %q", s)
	}
}

func TestRequestStatus(t *testing.T) {
	body := `{"requestStatusPerDestination":[{"requestStatus":"FAILED","errorInfo":{"errorCounts":[{"recordCount":"1","reason":"PROCESSING_ERROR_REASON_EXPIRED_CLICK"}]},"eventsIngestionStatus":{"recordCount":"1"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/requestStatus:retrieve" || r.URL.Query().Get("requestId") != "a/b" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	d := DataManager{HTTP: srv.Client(), Base: srv.URL, AccessToken: "tok"}
	st, err := d.RequestStatus(context.Background(), "a/b")
	if err != nil || st.Status != DMFailed || len(st.Errors) != 1 || !strings.Contains(PlainReason(st.Errors[0]), "click window") {
		t.Errorf("%+v %v", st, err)
	}
}
