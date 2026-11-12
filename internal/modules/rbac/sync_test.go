package rbac

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// memRepo is an in-memory Repository for the sync tests; methods the sync does not use panic
// through the nil embedded interface.
type memRepo struct {
	Repository
	users       []*MarketflowUser
	roles       []*MarketflowRole
	assignments []*UserRoleAssignment
}

var errNotFound = errors.New("not found")

func (m *memRepo) CreateUser(_ context.Context, _ uuid.UUID, u *MarketflowUser) error {
	for _, x := range m.users {
		if x.ID == u.ID {
			return errors.New("duplicate id")
		}
	}
	m.users = append(m.users, u)
	return nil
}
func (m *memRepo) GetUserByAuthServiceID(_ context.Context, t, a uuid.UUID) (*MarketflowUser, error) {
	for _, u := range m.users {
		if u.TenantID == t && u.AuthServiceUserID == a {
			return u, nil
		}
	}
	return nil, errNotFound
}
func (m *memRepo) UpdateUser(_ context.Context, _ uuid.UUID, id uuid.UUID, up *UserUpdates) error {
	for _, u := range m.users {
		if u.ID == id {
			if up.Status != nil {
				u.Status = *up.Status
			}
			if up.SyncStatus != nil {
				u.SyncStatus = *up.SyncStatus
			}
		}
	}
	return nil
}
func (m *memRepo) DeactivateUser(_ context.Context, _ uuid.UUID, id uuid.UUID) error {
	for _, u := range m.users {
		if u.ID == id {
			u.Status = "deactivated"
		}
	}
	return nil
}
func (m *memRepo) CreateRole(_ context.Context, _ uuid.UUID, r *MarketflowRole) error {
	m.roles = append(m.roles, r)
	return nil
}
func (m *memRepo) GetRoleByCode(_ context.Context, t uuid.UUID, code string) (*MarketflowRole, error) {
	for _, r := range m.roles {
		if r.TenantID == t && r.RoleCode == code {
			return r, nil
		}
	}
	return nil, errNotFound
}
func (m *memRepo) AssignRoleToUser(_ context.Context, _ uuid.UUID, a *UserRoleAssignment) error {
	m.assignments = append(m.assignments, a)
	return nil
}
func (m *memRepo) RevokeRoleFromUser(_ context.Context, _ uuid.UUID, user, role uuid.UUID) error {
	out := m.assignments[:0]
	for _, a := range m.assignments {
		if !(a.UserID == user && a.RoleID == role) {
			out = append(out, a)
		}
	}
	m.assignments = out
	return nil
}
func (m *memRepo) GetUserRoles(_ context.Context, t, user uuid.UUID) ([]*MarketflowRole, error) {
	var out []*MarketflowRole
	for _, a := range m.assignments {
		if a.UserID != user {
			continue
		}
		for _, r := range m.roles {
			if r.ID == a.RoleID && r.TenantID == t {
				out = append(out, r)
			}
		}
	}
	return out, nil
}
func (m *memRepo) ListUserAssignments(_ context.Context, _ uuid.UUID, f AssignmentFilters) ([]*UserRoleAssignment, error) {
	var out []*UserRoleAssignment
	for _, a := range m.assignments {
		if f.UserID != nil && a.UserID != *f.UserID {
			continue
		}
		if f.RoleID != nil && a.RoleID != *f.RoleID {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func roleOf(t *testing.T, s *Service, repo *memRepo, tenant, auth uuid.UUID) string {
	t.Helper()
	u, err := repo.GetUserByAuthServiceID(context.Background(), tenant, auth)
	if err != nil {
		t.Fatalf("no local user: %v", err)
	}
	rs, _ := repo.GetUserRoles(context.Background(), tenant, u.ID)
	if len(rs) != 1 {
		t.Fatalf("want exactly one role, got %d", len(rs))
	}
	return rs[0].RoleCode
}

// Security audit 2026-11-10 (MF #7): the local user and role sync was never called, so
// production had one local user and every tenant's own staff failed every role gate. The sync
// now runs for each signed-in caller: highest SSO role wins, plain staff and members are viewers,
// and the tenant's system roles are created when missing.
func TestSyncCallerMapsHighestRole(t *testing.T) {
	cases := []struct {
		roles []string
		want  string
	}{
		{[]string{"member"}, "viewer"},
		{[]string{"staff"}, "viewer"},
		{[]string{"cashier"}, "viewer"},
		{nil, "viewer"},
		{[]string{"marketflow_member"}, "fbo_owner"},
		{[]string{"member", "marketflow_member"}, "fbo_owner"},
		{[]string{"member", "admin"}, "marketflow_admin"},
		{[]string{"superuser"}, "marketflow_admin"},
		{[]string{"owner"}, "marketflow_admin"},
		{[]string{"marketflow_admin"}, "marketflow_admin"},
		// The Meta App Review account: above a viewer, below a member.
		{[]string{"marketflow_reviewer"}, "app_reviewer"},
		{[]string{"member", "marketflow_reviewer"}, "app_reviewer"},
		{[]string{"marketflow_reviewer", "marketflow_member"}, "fbo_owner"},
		{[]string{"marketflow_reviewer", "admin"}, "marketflow_admin"},
		{[]string{"MarketFlow_Reviewer "}, "app_reviewer"},
	}
	for _, c := range cases {
		repo := &memRepo{}
		s := NewService(repo, zap.NewNop())
		tenant, auth := uuid.New(), uuid.New()
		if err := s.SyncCaller(context.Background(), tenant, auth, "a@b.c", c.roles); err != nil {
			t.Fatalf("%v: %v", c.roles, err)
		}
		if got := roleOf(t, s, repo, tenant, auth); got != c.want {
			t.Errorf("%v: got %s want %s", c.roles, got, c.want)
		}
		if len(repo.roles) != len(systemRoleDefs) {
			t.Errorf("system roles created once each: %d", len(repo.roles))
		}
	}
}

func TestSyncCallerFollowsSSOButKeepsManualGrants(t *testing.T) {
	repo := &memRepo{}
	s := NewService(repo, zap.NewNop())
	ctx := context.Background()
	tenant, auth := uuid.New(), uuid.New()

	_ = s.SyncCaller(ctx, tenant, auth, "a@b.c", []string{"admin"})
	_ = s.SyncCaller(ctx, tenant, auth, "a@b.c", []string{"member"})
	if got := roleOf(t, s, repo, tenant, auth); got != "viewer" {
		t.Fatalf("demoted in SSO: sync-made role follows, got %s", got)
	}

	// A MarketFlow admin promotes them by hand; the next sync keeps that.
	u, _ := repo.GetUserByAuthServiceID(ctx, tenant, auth)
	if err := s.UpdateUserRole(ctx, tenant, u.ID, "fbo_owner", uuid.New()); err != nil {
		t.Fatal(err)
	}
	_ = s.SyncCaller(ctx, tenant, auth, "a@b.c", []string{"member"})
	if got := roleOf(t, s, repo, tenant, auth); got != "fbo_owner" {
		t.Fatalf("manual grant kept, got %s", got)
	}
}

// MF #2: a removed (deactivated) member fails every role gate and is not reactivated by a sync.
func TestDeactivatedUserFailsRoleGates(t *testing.T) {
	repo := &memRepo{}
	s := NewService(repo, zap.NewNop())
	ctx := context.Background()
	tenant, auth := uuid.New(), uuid.New()
	_ = s.SyncCaller(ctx, tenant, auth, "a@b.c", []string{"admin"})
	u, _ := repo.GetUserByAuthServiceID(ctx, tenant, auth)
	_ = s.DeactivateUser(ctx, tenant, u.ID)

	_ = s.SyncCaller(ctx, tenant, auth, "a@b.c", []string{"admin"})
	if u.Status != "deactivated" {
		t.Fatalf("sync reactivated a removed member: %s", u.Status)
	}
	c := &authclient.Claims{TenantID: tenant.String()}
	c.Subject = auth.String()
	claimsCtx := authclient.ContextWithClaims(ctx, c)
	if s.CallerHasAnyRole(claimsCtx, tenant, "marketflow_admin") {
		t.Fatal("a deactivated user passed a role gate")
	}
}

// The same SSO person in two tenants gets one local user per tenant.
func TestSyncCallerSamePersonTwoTenants(t *testing.T) {
	repo := &memRepo{}
	s := NewService(repo, zap.NewNop())
	auth := uuid.New()
	if err := s.SyncCaller(context.Background(), uuid.New(), auth, "a@b.c", []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncCaller(context.Background(), uuid.New(), auth, "a@b.c", []string{"member"}); err != nil {
		t.Fatalf("second tenant: %v", err)
	}
	if len(repo.users) != 2 {
		t.Fatalf("want 2 local users, got %d", len(repo.users))
	}
}

func TestSyncMiddleware(t *testing.T) {
	repo := &memRepo{}
	s := NewService(repo, zap.NewNop())
	tenant, auth := uuid.New(), uuid.New()
	var reached int
	h := s.SyncMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))

	serve := func(c *authclient.Claims) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		h.ServeHTTP(httptest.NewRecorder(), r.WithContext(authclient.ContextWithClaims(r.Context(), c)))
	}
	user := &authclient.Claims{TenantID: tenant.String(), Roles: []string{"admin"}}
	user.Subject = auth.String()
	serve(user)
	if got := roleOf(t, s, repo, tenant, auth); got != "marketflow_admin" {
		t.Fatalf("first request syncs the caller: %s", got)
	}

	svc := &authclient.Claims{TenantID: tenant.String(), IsService: true}
	svc.Subject = uuid.NewString()
	serve(svc)
	if len(repo.users) != 1 {
		t.Fatalf("service keys are not users: %d", len(repo.users))
	}

	// Same roles again: served from the cache, no further repository writes.
	n := len(repo.assignments)
	serve(user)
	if len(repo.assignments) != n || reached != 3 {
		t.Fatalf("cached sync: assignments %d->%d, reached %d", n, len(repo.assignments), reached)
	}
}
