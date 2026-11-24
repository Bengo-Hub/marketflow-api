package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDeliveryEstimate(t *testing.T) {
	var q url.Values
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, path = r.URL.Query(), r.URL.Path
		_, _ = w.Write([]byte(`{"data":[{"estimate_ready":true,"estimate_dau":5000,"daily_outcomes_curve":[{"spend":10000,"reach":1200,"impressions":2000,"actions":3}]}]}`))
	}))
	defer srv.Close()
	c := NewClient("")
	c.baseURL = srv.URL
	est, err := c.DeliveryEstimate(context.Background(), "tok", "123", "CONVERSATIONS", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/act_123/delivery_estimate") || q.Get("optimization_goal") != "CONVERSATIONS" ||
		!strings.Contains(q.Get("targeting_spec"), `"KE"`) {
		t.Fatalf("path %s query %v", path, q)
	}
	if !est.Ready || len(est.Curve) != 1 || est.Curve[0].Reach != 1200 || est.Curve[0].Spend != 10000 {
		t.Fatalf("estimate = %+v", est)
	}
}
