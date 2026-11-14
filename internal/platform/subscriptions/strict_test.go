package subscriptions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Sprint 18 flag (2026-11-10): paid_ads_launch (no launch charge) went through the fail-open
// consumer gate, so a subscriptions-api outage made every ad launch free. A benefit that waives a
// charge is granted only when the plan lists it. Owner, 2026-11-12: the public demo tenant gets
// free plan features only, so the demo bypass no longer grants it.
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
	if c.StrictHasFeature(ctx, "demo", "paid_ads_launch") {
		t.Error("the demo bypass must not waive the launch charge")
	}
	if c.StrictHasFeature(ctx, "outage", "paid_ads_launch") {
		t.Error("an outage must not waive the charge")
	}
	var nilClient *Client
	if nilClient.StrictHasFeature(ctx, "with", "paid_ads_launch") {
		t.Error("not wired means not granted")
	}
}

// max_autopilot_agents (owner, 2026-11-14) is read strictly: an outage is not known, a plan
// without the key says so, and -1 comes through as unlimited.
func TestStrictLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tenants/lim-three/subscription":
			_, _ = w.Write([]byte(`{"features":["marketflow_autopilot"],"limits":{"max_autopilot_agents":3}}`))
		case "/api/v1/tenants/lim-unlimited/subscription":
			_, _ = w.Write([]byte(`{"features":["marketflow_autopilot"],"limits":{"max_autopilot_agents":-1}}`))
		case "/api/v1/tenants/lim-none/subscription":
			_, _ = w.Write([]byte(`{"features":[],"limits":{}}`))
		default:
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	c := NewClient(Config{ServiceURL: srv.URL})
	ctx := context.Background()
	if n, has, known := c.StrictLimit(ctx, "lim-three", "max_autopilot_agents"); n != 3 || !has || !known {
		t.Errorf("three: %d %v %v", n, has, known)
	}
	if n, has, known := c.StrictLimit(ctx, "lim-unlimited", "max_autopilot_agents"); n != -1 || !has || !known {
		t.Errorf("unlimited: %d %v %v", n, has, known)
	}
	if _, has, known := c.StrictLimit(ctx, "lim-none", "max_autopilot_agents"); has || !known {
		t.Errorf("no key: has=%v known=%v", has, known)
	}
	if _, _, known := c.StrictLimit(ctx, "lim-outage", "max_autopilot_agents"); known {
		t.Error("an outage must not be a known answer")
	}
	var nilClient *Client
	if _, _, known := nilClient.StrictLimit(ctx, "lim-three", "max_autopilot_agents"); known {
		t.Error("not wired is not known")
	}
}
