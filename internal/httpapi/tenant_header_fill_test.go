package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type stubSyncer map[string]uuid.UUID

func (s stubSyncer) SyncTenant(_ context.Context, slug string) (uuid.UUID, error) {
	return s[slug], nil
}

// Handlers read X-Tenant-ID; a client that sends only the slug must still reach its tenant.
func TestTenantHeaderFilledFromSlugOrToken(t *testing.T) {
	acme, rival := uuid.New(), uuid.New()
	pass := func(next http.Handler) http.Handler { return next }
	var withClaims *authclient.Claims
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if withClaims != nil {
				r = r.WithContext(authclient.ContextWithClaims(r.Context(), withClaims))
			}
			next.ServeHTTP(w, r)
		})
	}
	var seen string
	h := NewRouter(RouterDeps{
		Log: zap.NewNop(), RequireAuthHandler: auth, TenantSyncer: stubSyncer{"acme": acme, "rival": rival},
		RequirePlatformOwner: pass, RequireAICredits: pass, RequireCampaignSlot: pass,
		TrackLeadCreated: pass, TrackCampaignCreated: pass, TrackMeetingBooked: pass,
		AnalyticsHandlers: AnalyticsHandlers{Dashboard: func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("X-Tenant-ID")
			w.WriteHeader(http.StatusOK)
		}},
	})
	get := func(claims *authclient.Claims, hdr map[string]string) string {
		withClaims, seen = claims, ""
		req := httptest.NewRequest(http.MethodGet, "/api/v1/analytics/dashboard", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return seen
	}
	owner := &authclient.Claims{TenantID: uuid.NewString(), TenantSlug: "codevertex", IsPlatformOwner: true}
	user := &authclient.Claims{TenantID: acme.String(), TenantSlug: "acme", ActiveServiceTags: []string{"marketflow"}}

	if got := get(owner, map[string]string{"X-Tenant-Slug": "rival"}); got != rival.String() {
		t.Errorf("a platform owner sending only the slug acts for that tenant: %q", got)
	}
	if got := get(user, nil); got != acme.String() {
		t.Errorf("a tenant user with no headers gets their own tenant: %q", got)
	}
	if got := get(user, map[string]string{"X-Tenant-Slug": "rival"}); got != acme.String() {
		t.Errorf("a tenant user naming another slug still gets their own tenant: %q", got)
	}
	explicit := uuid.NewString()
	if got := get(owner, map[string]string{"X-Tenant-ID": explicit, "X-Tenant-Slug": "rival"}); got != explicit {
		t.Errorf("an explicit X-Tenant-ID is left alone: %q", got)
	}
	if got := get(user, map[string]string{"X-Tenant-ID": "undefined"}); got != acme.String() {
		t.Errorf("a non-UUID X-Tenant-ID is replaced by the token tenant: %q", got)
	}
	if got := get(owner, map[string]string{"X-Tenant-ID": "undefined", "X-Tenant-Slug": "rival"}); got != rival.String() {
		t.Errorf("a non-UUID X-Tenant-ID is replaced by the slug tenant: %q", got)
	}
	if got := get(owner, map[string]string{"X-Tenant-Slug": "unknown"}); got != "" {
		t.Errorf("an unknown slug resolves to nothing: %q", got)
	}
}
