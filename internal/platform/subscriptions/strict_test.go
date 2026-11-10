package subscriptions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Sprint 18 flag (2026-11-10): paid_ads_launch (no launch charge) went through the fail-open
// consumer gate, so a subscriptions-api outage made every ad launch free. A benefit that waives a
// charge is granted only when the plan lists it (or the tenant is a demo).
func TestStrictHasFeature(t *testing.T) {
	body := `{"features":["paid_ads_launch"],"status":"ACTIVE"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tenants/with/subscription":
			_, _ = w.Write([]byte(body))
		case "/api/v1/tenants/without/subscription":
			_, _ = w.Write([]byte(`{"features":[],"billing_mode":"service_charge"}`))
		case "/api/v1/tenants/demo/subscription":
			_, _ = w.Write([]byte(`{"features":[],"is_demo_bypass":true}`))
		default:
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	c := NewClient(Config{ServiceURL: srv.URL})
	ctx := context.Background()
	if !c.StrictHasFeature(ctx, "with", "paid_ads_launch") {
		t.Error("listed feature is granted")
	}
	if c.StrictHasFeature(ctx, "without", "paid_ads_launch") {
		t.Error("PAYG without the feature pays the launch charge")
	}
	if !c.StrictHasFeature(ctx, "demo", "paid_ads_launch") {
		t.Error("demo tenants are exempt")
	}
	if c.StrictHasFeature(ctx, "outage", "paid_ads_launch") {
		t.Error("an outage must not waive the charge")
	}
	var nilClient *Client
	if nilClient.StrictHasFeature(ctx, "with", "paid_ads_launch") {
		t.Error("not wired means not granted")
	}
}
