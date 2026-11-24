package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

func TestBillingURL(t *testing.T) {
	cases := []struct{ provider, configured, want string }{
		{"fal", "", "https://fal.ai/dashboard/billing"},
		{"FAL", "", "https://fal.ai/dashboard/billing"},
		{"openai", "", "https://platform.openai.com/settings/organization/billing/overview"},
		{"fal", "https://fal.ai/dashboard/usage-billing/credits", "https://fal.ai/dashboard/usage-billing/credits"},
		{"fal", "http://fal.ai/billing", "https://fal.ai/dashboard/billing"},       // not https: default
		{"fal", "javascript:alert(1)", "https://fal.ai/dashboard/billing"},          // never a script link
		{"fal", "https://user:pw@evil.example", "https://fal.ai/dashboard/billing"}, // no credentials in a link
		{"creatomate", "https://creatomate.com/billing", "https://creatomate.com/billing"},
		{"unknown", "", ""},
	}
	for _, c := range cases {
		if got := BillingURL(c.provider, c.configured); got != c.want {
			t.Errorf("BillingURL(%q, %q) = %q, want %q", c.provider, c.configured, got, c.want)
		}
	}
}

func TestWithBillingDefault(t *testing.T) {
	s := map[string]string{"api_key": "k"}
	withBillingDefault("elevenlabs", s)
	if s[BillingURLKey] != "https://elevenlabs.io/app/subscription" {
		t.Fatalf("default not added: %v", s)
	}
	s = map[string]string{BillingURLKey: "https://example.com/pay"}
	withBillingDefault("elevenlabs", s)
	if s[BillingURLKey] != "https://example.com/pay" {
		t.Fatalf("configured link replaced: %v", s)
	}
	s = map[string]string{BillingURLKey: "ftp://x"}
	withBillingDefault("unknown", s)
	if _, ok := s[BillingURLKey]; ok {
		t.Fatalf("an invalid link with no default is dropped: %v", s)
	}
}

func TestCheckBillingURL(t *testing.T) {
	w := httptest.NewRecorder()
	if !checkBillingURL(w, map[string]string{"api_key": "k"}, zap.NewNop()) {
		t.Fatal("no billing_url is fine")
	}
	if checkBillingURL(w, map[string]string{BillingURLKey: "http://x.example"}, zap.NewNop()) || w.Code != http.StatusBadRequest {
		t.Fatalf("http link accepted (code %d)", w.Code)
	}
}

func TestProviderBillingHandler(t *testing.T) {
	h := &PlatformProviderHandler{logger: zap.NewNop()} // no DB: defaults only
	get := func(p string) (*httptest.ResponseRecorder, map[string]string) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("provider", p)
		r := httptest.NewRequest(http.MethodGet, "/api/v1/public/provider-billing/"+p, nil)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		h.ProviderBilling(w, r)
		var out map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w, out
	}
	if w, out := get("higgsfield"); w.Code != http.StatusOK || out["url"] != "https://console.higgsfield.ai" {
		t.Fatalf("higgsfield: %d %v", w.Code, out)
	}
	if w, _ := get("nobody"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", w.Code)
	}
	if w, _ := get("../etc"); w.Code != http.StatusNotFound {
		t.Fatalf("bad name: %d", w.Code)
	}
}
