package rbac

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// systemRoleDefs are the MarketFlow roles every tenant has. Created on first sync when missing
// (the seed command only ever created them for the platform tenant).
var systemRoleDefs = []struct{ code, name, desc string }{
	{RoleTenantAdmin, "MarketFlow Admin", "Settings, integrations, team, autopilot budgets, lead export and delete"},
	{RoleMember, "Member", "Day-to-day marketing: create, approve, boost, post, reply, spend credits"},
	{RoleViewer, "Viewer", "Read only"},
}

const (
	// RoleMember is the day-to-day marketing role (kept as fbo_owner for existing data).
	RoleMember = "fbo_owner"
	// RoleViewer is read only.
	RoleViewer = "viewer"
	statusDeactivated = "deactivated"
)

// mapGlobalRoleToMarketflowRole picks the MarketFlow role for a caller's SSO roles in the tenant.
// The highest role wins whatever the order. Organisation admins own MarketFlow too; the
// MarketFlow-scoped SSO roles map directly; everyone else (POS cashiers, plain members, staff)
// is a viewer until someone grants more.
func mapGlobalRoleToMarketflowRole(roles []string) string {
	rank := map[string]int{RoleViewer: 0, RoleMember: 1, RoleTenantAdmin: 2}
	best := RoleViewer
	for _, r := range roles {
		var code string
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "superuser", "admin", "owner", "marketflow_admin":
			code = RoleTenantAdmin
		case "marketflow_member":
			code = RoleMember
		default:
			continue
		}
		if rank[code] > rank[best] {
			best = code
		}
	}
	return best
}

// ensureSystemRoles creates the tenant's missing system roles.
func (s *Service) ensureSystemRoles(ctx context.Context, tenantID uuid.UUID) error {
	for _, d := range systemRoleDefs {
		if _, err := s.repo.GetRoleByCode(ctx, tenantID, d.code); err == nil {
			continue
		}
		desc := d.desc
		if err := s.repo.CreateRole(ctx, tenantID, &MarketflowRole{
			ID: uuid.New(), TenantID: tenantID, RoleCode: d.code, Name: d.name,
			Description: &desc, IsSystemRole: true,
		}); err != nil {
			// A concurrent first request may have created it; only fail when it is still missing.
			if _, again := s.repo.GetRoleByCode(ctx, tenantID, d.code); again != nil {
				return fmt.Errorf("create role %s: %w", d.code, err)
			}
		}
	}
	return nil
}

// SyncCaller keeps a signed-in caller's local MarketFlow user and role in step with SSO. It
// creates the user on first sight, and sets the SSO-derived role. A role the sync set itself
// (assigned by the user's own SSO id) follows SSO on later requests; a role a MarketFlow admin
// granted by hand is kept. A deactivated (removed) user is left as is.
func (s *Service) SyncCaller(ctx context.Context, tenantID, authUserID uuid.UUID, email string, ssoRoles []string) error {
	if tenantID == uuid.Nil || authUserID == uuid.Nil {
		return nil
	}
	user, err := s.repo.GetUserByAuthServiceID(ctx, tenantID, authUserID)
	if err != nil {
		user = &MarketflowUser{
			ID: uuid.New(), TenantID: tenantID, AuthServiceUserID: authUserID,
			Email: email, Status: "active", SyncStatus: "synced",
		}
		if cErr := s.repo.CreateUser(ctx, tenantID, user); cErr != nil {
			// Lost a race with a parallel request: use the row it created.
			if existing, gErr := s.repo.GetUserByAuthServiceID(ctx, tenantID, authUserID); gErr == nil {
				user = existing
			} else {
				return fmt.Errorf("create user: %w", cErr)
			}
		} else {
			s.logger.Info("user synced", zap.String("tenant_id", tenantID.String()), zap.String("user_id", user.ID.String()))
		}
	}
	if user.Status == statusDeactivated {
		return nil
	}
	if err := s.ensureSystemRoles(ctx, tenantID); err != nil {
		return err
	}
	want := mapGlobalRoleToMarketflowRole(ssoRoles)

	assignments, err := s.repo.ListUserAssignments(ctx, tenantID, AssignmentFilters{UserID: &user.ID})
	if err != nil {
		return err
	}
	for _, a := range assignments {
		if a.AssignedBy != authUserID {
			return nil // granted by a MarketFlow admin: keep it
		}
	}
	current, err := s.repo.GetUserRoles(ctx, tenantID, user.ID)
	if err != nil {
		return err
	}
	if len(current) == 1 && current[0].RoleCode == want {
		return nil
	}
	role, err := s.repo.GetRoleByCode(ctx, tenantID, want)
	if err != nil {
		return fmt.Errorf("role %s: %w", want, err)
	}
	for _, r := range current {
		_ = s.repo.RevokeRoleFromUser(ctx, tenantID, user.ID, r.ID)
	}
	if err := s.repo.AssignRoleToUser(ctx, tenantID, &UserRoleAssignment{
		ID: uuid.New(), TenantID: tenantID, UserID: user.ID, RoleID: role.ID, AssignedBy: authUserID,
	}); err != nil {
		return fmt.Errorf("assign %s: %w", want, err)
	}
	s.logger.Info("synced marketflow role", zap.String("role_code", want),
		zap.String("user_id", user.ID.String()), zap.String("tenant_id", tenantID.String()))
	return nil
}
