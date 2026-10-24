package tenant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// With no signed-in caller, every change is refused before anything is read or sent.
func TestMemberChangesNeedAnAdmin(t *testing.T) {
	h := NewMembersHandler(nil, nil, "http://auth.invalid", "k", zap.NewNop())
	tenant := "11111111-1111-1111-1111-111111111111"
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		run    http.HandlerFunc
	}{
		{"invite", http.MethodPost, "/api/v1/tenant/members/invite", `{"email":"a@b.co","role":"viewer"}`, h.InviteMember},
		{"role", http.MethodPut, "/api/v1/tenant/members/22222222-2222-2222-2222-222222222222", `{"role":"marketflow_admin"}`, h.UpdateMemberRole},
		{"remove", http.MethodDelete, "/api/v1/tenant/members/22222222-2222-2222-2222-222222222222", ``, h.RemoveMember},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("X-Tenant-ID", tenant)
		rec := httptest.NewRecorder()
		if c.name != "invite" {
			req = withUserIDParam(req, "22222222-2222-2222-2222-222222222222")
		}
		c.run(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", c.name, rec.Code)
		}
	}
}

func withUserIDParam(r *http.Request, id string) *http.Request {
	rc := chi.NewRouteContext()
	rc.URLParams.Add("userId", id)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
}

func TestInviteRolesCoverAssignableRoles(t *testing.T) {
	for _, r := range rbac.AssignableRoles {
		if _, ok := inviteRoles[r]; !ok {
			t.Errorf("role %q can be assigned but not invited", r)
		}
	}
	for _, roles := range inviteRoles {
		for _, r := range roles {
			if r == "superuser" {
				t.Fatal("an invite must never grant superuser")
			}
		}
	}
}
