package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// The provider redirects the browser to the callback without a bearer token, so the callbacks
// must be public while the rest of /integrations stays behind sign-in.
func TestOAuthCallbacksArePublic(t *testing.T) {
	stub := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}
	pass := func(next http.Handler) http.Handler { return next }
	h := NewRouter(RouterDeps{
		Log:                  zap.NewNop(),
		RequireAuthHandler:   deny,
		RequirePlatformOwner: pass, RequireAICredits: pass, RequireCampaignSlot: pass,
		TrackLeadCreated: pass, TrackCampaignCreated: pass, TrackMeetingBooked: pass,
		IntegrationsHandlers: IntegrationsHandlers{
			GoogleCallback: stub, MetaCallback: stub, TikTokCallback: stub,
			MetaAuthorize: stub, ListTenant: stub,
		},
	})
	for path, want := range map[string]int{
		"/api/v1/integrations/google/callback?code=x&state=y": http.StatusTeapot,
		"/api/v1/integrations/meta/callback?code=x&state=y":   http.StatusTeapot,
		"/api/v1/integrations/tiktok/callback?auth_code=x":    http.StatusTeapot,
		"/api/v1/integrations/meta/authorize":                 http.StatusUnauthorized,
		"/api/v1/integrations/":                               http.StatusUnauthorized,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
}
