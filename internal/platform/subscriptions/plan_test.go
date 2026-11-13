package subscriptions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The demo tenant gets exactly the free plan's features and limits (owner, 2026-11-12); the
// plan is read from subscriptions-api, never written here.
func TestGetPlan(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/plans/code/MARKETFLOW_STARTER_T" {
			http.NotFound(w, r)
			return
		}
		calls++
		_, _ = w.Write([]byte(`{"plan":{"planCode":"MARKETFLOW_STARTER_T","tierLimits":{"chat_turns_daily":10,"max_leads":50},
			"features":[{"featureCode":"ai_static_ads","isIncluded":true},{"featureCode":"white_label","isIncluded":false}]}}`))
	}))
	defer srv.Close()
	c := NewClient(Config{ServiceURL: srv.URL})
	p := c.GetPlan(context.Background(), "MARKETFLOW_STARTER_T")
	if p == nil || p.Limits["chat_turns_daily"] != 10 || p.Limits["max_leads"] != 50 || len(p.Features) != 1 || p.Features[0] != "ai_static_ads" {
		t.Fatalf("plan %+v", p)
	}
	if c.GetPlan(context.Background(), "MARKETFLOW_STARTER_T"); calls != 1 {
		t.Fatalf("the plan is cached, fetched %d times", calls)
	}
	if c.GetPlan(context.Background(), "MISSING") != nil {
		t.Fatal("an unknown plan is nil")
	}
}
