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
)

// The Ads calls send the bearer and no developer token, use the configured version, and read
// Google's JSON shapes (numbers as strings, cost in micros, partial failures).
func TestAdsCalls(t *testing.T) {
	var gotQuery, gotLogin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("developer-token") != "" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":{"message":"bad headers"}}`, http.StatusUnauthorized)
			return
		}
		gotLogin = r.Header.Get("login-customer-id")
		switch {
		case r.URL.Path == "/v99/customers/333/googleAds:search":
			http.Error(w, `{"error":{"code":403,"message":"The caller does not have permission","details":[{"errors":[{"errorCode":{"authorizationError":"CLOUD_PROJECT_NOT_APPROVED_FOR_PRODUCTION"}}]}]}}`, http.StatusForbidden)
		case r.URL.Path == "/v99/customers:listAccessibleCustomers":
			_, _ = w.Write([]byte(`{"resourceNames":["customers/111","customers/222"]}`))
		case strings.HasSuffix(r.URL.Path, "/googleAds:search"):
			b, _ := io.ReadAll(r.Body)
			var q map[string]string
			_ = json.Unmarshal(b, &q)
			gotQuery = q["query"]
			if strings.Contains(gotQuery, "FROM customer") {
				_, _ = w.Write([]byte(`{"results":[{"customer":{"descriptiveName":"Boi Shop","currencyCode":"KES","manager":false}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"campaign":{"id":"7","name":"Search","status":"ENABLED","advertisingChannelType":"SEARCH"},
				"metrics":{"impressions":"1200","clicks":"40","conversions":3.5,"costMicros":"2500000"}}]}`))
		case strings.HasSuffix(r.URL.Path, ":uploadClickConversions"):
			_, _ = w.Write([]byte(`{"partialFailureError":{"message":"gclid not found"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	// A developer_token left on the row is ignored: Google retired them on 2026-09-09.
	ads := AdsFromSettings(srv.Client(), map[string]string{"developer_token": "dev", "api_version": "v99", "login_customer_id": "932-084-9278"}, "tok")
	ads.Base = srv.URL
	ctx := context.Background()

	ids, err := ads.AccessibleCustomers(ctx)
	if err != nil || len(ids) != 2 || ids[0] != "111" || gotLogin != "9320849278" {
		t.Fatalf("customers: %v %v login=%q", ids, err, gotLogin)
	}
	c, err := ads.CustomerInfo(ctx, "111")
	if err != nil || c.Name != "Boi Shop" || c.Currency != "KES" {
		t.Fatalf("customer: %+v %v", c, err)
	}
	rows, err := ads.CampaignResults(ctx, "111", "DROP TABLE")
	if err != nil || len(rows) != 1 || rows[0].Impressions != 1200 || rows[0].Cost != 2.5 || !strings.Contains(gotQuery, "DURING LAST_30_DAYS") {
		t.Fatalf("campaigns: %+v %v %q", rows, err, gotQuery)
	}
	err = ads.UploadClickConversions(ctx, "111", []ClickConversion{{GCLID: "g", ConversionAction: "customers/111/conversionActions/1"}})
	if err == nil || !strings.Contains(err.Error(), "gclid not found") {
		t.Fatalf("partial failure must surface: %v", err)
	}
	_, err = ads.CustomerInfo(ctx, "333")
	var ae *AdsError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || ae.Message != ProjectAccessMessage {
		t.Fatalf("Test-only project access must read as the Cloud Console step: %v", err)
	}
}
