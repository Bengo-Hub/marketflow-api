package rbac

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Audit H2: a service key that names a person (X-Acting-User-ID) is held to that person's local
// role in the tenant; spend and publish writes need a named member or admin.
func TestWriteGateServiceActingUser(t *testing.T) {
	repo := &memRepo{}
	s := NewService(repo, zap.NewNop())
	tenant, other := uuid.New(), uuid.New()
	viewer, member, admin, reviewer, outsider := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	_ = s.SyncCaller(t.Context(), tenant, viewer, "v@x", []string{"member"})
	_ = s.SyncCaller(t.Context(), tenant, member, "m@x", []string{"marketflow_member"})
	_ = s.SyncCaller(t.Context(), tenant, admin, "a@x", []string{"marketflow_admin"})
	_ = s.SyncCaller(t.Context(), tenant, reviewer, "r@x", []string{"member", "marketflow_reviewer"})
	// outsider is a member of another tenant only.
	_ = s.SyncCaller(t.Context(), other, outsider, "o@x", []string{"marketflow_admin"})

	var seenActor uuid.UUID
	gate := s.WriteGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenActor, _ = httpapi.ActingUser(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	const job = "7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b"
	cases := []struct {
		name         string
		method, path string
		actor        string // X-Acting-User-ID
		origin       string // X-Acting-Origin
		want         int
		wantErr      string
	}{
		{"no actor reads", "GET", "/api/v1/leads", "", "", 200, ""},
		{"no actor writes a lead as before", "POST", "/api/v1/leads", "", "", 200, ""},
		{"no actor cannot boost", "POST", "/api/v1/ads/boosts", "", "", 403, "acting_user_required"},
		{"no actor cannot ask a boost code", "POST", "/api/v1/ads/boosts/otp", "", "", 403, "acting_user_required"},
		{"no actor cannot confirm paid", "POST", "/api/v1/ads/boosts/" + job + "/paid", "", "", 403, "acting_user_required"},
		{"no actor cannot resume", "POST", "/api/v1/ads/boosts/" + job + "/resume", "", "", 403, "acting_user_required"},
		{"no actor cannot resume an ad", "POST", "/api/v1/ads/boosts/" + job + "/ads/9/resume", "", "", 403, "acting_user_required"},
		{"no actor may pause", "POST", "/api/v1/ads/boosts/" + job + "/pause", "", "", 200, ""},
		{"no actor may quote", "POST", "/api/v1/ads/quote", "", "", 200, ""},
		{"no actor cannot post", "POST", "/api/v1/publishing/jobs", "", "", 403, "acting_user_required"},
		{"no actor cannot retry a post", "POST", "/api/v1/publishing/jobs/" + job + "/retry", "", "", 403, "acting_user_required"},
		{"no actor cannot send sms", "POST", "/api/v1/" + tenant.String() + "/campaigns/c1/send-sms", "", "", 403, "acting_user_required"},
		{"an unknown origin is not enough", "POST", "/api/v1/ads/boosts", "", "worker", 403, "acting_user_required"},
		{"a bad actor id", "POST", "/api/v1/leads", "not-a-uuid", "", 400, "invalid_acting_user"},

		{"member actor boosts", "POST", "/api/v1/ads/boosts", member.String(), "", 200, ""},
		{"admin actor posts", "POST", "/api/v1/publishing/jobs", admin.String(), "", 200, ""},
		{"member actor sends sms", "POST", "/api/v1/" + tenant.String() + "/campaigns/c1/send-sms", member.String(), "", 200, ""},
		{"viewer actor reads", "GET", "/api/v1/leads", viewer.String(), "", 200, ""},
		{"viewer actor cannot write", "POST", "/api/v1/leads", viewer.String(), "", 403, "role_required"},
		{"viewer actor cannot boost", "POST", "/api/v1/ads/boosts", viewer.String(), "", 403, "role_required"},
		{"member actor cannot change settings", "PUT", "/api/v1/tenant/os-settings", member.String(), "", 403, "role_required"},
		{"admin actor changes settings", "PUT", "/api/v1/tenant/os-settings", admin.String(), "", 200, ""},
		{"reviewer actor cannot boost through a service", "POST", "/api/v1/ads/boosts", reviewer.String(), "", 403, "role_required"},
		{"reviewer actor keeps its allow-list", "GET", "/api/v1/contacts", reviewer.String(), "", 403, "not_available_for_role"},
		{"actor from another tenant is a viewer", "POST", "/api/v1/ads/boosts", outsider.String(), "", 403, "role_required"},
		{"actor from another tenant cannot write", "POST", "/api/v1/leads", outsider.String(), "", 403, "role_required"},
		{"sms checks the tenant in the path", "POST", "/api/v1/" + other.String() + "/campaigns/c1/send-sms", member.String(), "", 403, "role_required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seenActor = uuid.Nil
			claims := &authclient.Claims{TenantID: uuid.NewString(), IsService: true, ServiceName: "marketflow-ai"}
			r := httptest.NewRequest(c.method, c.path, nil)
			r.Header.Set("X-Tenant-ID", tenant.String())
			if c.actor != "" {
				r.Header.Set(httpapi.HeaderActingUserID, c.actor)
			}
			if c.origin != "" {
				r.Header.Set(httpapi.HeaderActingOrigin, c.origin)
			}
			r = r.WithContext(authclient.ContextWithClaims(r.Context(), claims))
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d (%s)", w.Code, c.want, w.Body.String())
			}
			if c.wantErr != "" {
				var body map[string]any
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if body["code"] != c.wantErr {
					t.Fatalf("code %v, want %s", body["code"], c.wantErr)
				}
			}
			if c.want == 200 && c.actor != "" && seenActor.String() != c.actor {
				t.Fatalf("acting user not passed on: %s", seenActor)
			}
		})
	}
}

// A platform owner is never asked for an acting user.
func TestWriteGatePlatformOwnerNeedsNoActor(t *testing.T) {
	s := NewService(&memRepo{}, zap.NewNop())
	gate := s.WriteGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	c := &authclient.Claims{TenantID: uuid.NewString(), IsPlatformOwner: true}
	c.Subject = uuid.NewString()
	r := httptest.NewRequest("POST", "/api/v1/ads/boosts", nil)
	r.Header.Set("X-Tenant-ID", uuid.NewString())
	r = r.WithContext(authclient.ContextWithClaims(r.Context(), c))
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("platform owner boost: %d", w.Code)
	}
}

func TestSpendOrPublish(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/api/v1/ads/boosts", true},
		{"POST", "/api/v1/ads/boosts/", true},
		{"POST", "/api/v1/ads/boosts/otp", true},
		{"GET", "/api/v1/ads/boosts/x", false},
		{"POST", "/api/v1/ads/boosts/x/pause", false},
		{"POST", "/api/v1/ads/boosts/x/paid", true},
		{"POST", "/api/v1/publishing/jobs/x/move", true},
		{"POST", "/api/v1/publishing/jobs/x/delete-from-platform", true},
		{"GET", "/api/v1/publishing/jobs", false},
		{"POST", "/api/v1/t1/campaigns/c1/send-sms", true},
		{"POST", "/api/v1/campaigns", false},
		{"POST", "/api/v1/creative/briefs", false},
	}
	for _, c := range cases {
		if got := spendOrPublish(c.method, c.path); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
