package rbac

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service provides business logic for RBAC operations.
type Service struct {
	repo   Repository
	logger *zap.Logger
}

// NewService creates a new RBAC service.
func NewService(repo Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("rbac"),
	}
}

// EnsureUserFromToken handles JIT provisioning of a user derived from an SSO token.
// It also assigns service-level roles based on the user's global roles from JWT claims.
func (s *Service) EnsureUserFromToken(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, email string, tenantSlug string, roles ...string) (*MarketflowUser, error) {
	// 1. If user already exists, return early.
	user, err := s.repo.GetUserByAuthServiceID(ctx, tenantID, userID)
	if err == nil {
		return user, nil
	}

	// 2. Create the user (JIT provisioning)
	user, err = s.SyncUser(ctx, tenantID, userID, email)
	if err != nil {
		return nil, err
	}

	// 3. Assign default service-level role based on global JWT roles
	s.assignDefaultRoleFromJWT(ctx, tenantID, user.ID, userID, roles)

	return user, nil
}

// assignDefaultRoleFromJWT maps global JWT roles to marketflow service-level roles.
// superuser/admin → marketflow_admin, staff/member → fbo_owner, others → viewer.
func (s *Service) assignDefaultRoleFromJWT(ctx context.Context, tenantID uuid.UUID, localUserID uuid.UUID, authUserID uuid.UUID, roles []string) {
	roleCode := mapGlobalRoleToMarketflowRole(roles)
	if roleCode == "" {
		return
	}

	role, err := s.repo.GetRoleByCode(ctx, tenantID, roleCode)
	if err != nil {
		s.logger.Debug("marketflow role not found for JIT assignment",
			zap.String("role_code", roleCode),
			zap.String("tenant_id", tenantID.String()),
		)
		return
	}

	// Idempotent: check if already assigned
	assignments, err := s.repo.ListUserAssignments(ctx, tenantID, AssignmentFilters{
		UserID: &localUserID,
		RoleID: &role.ID,
	})
	if err == nil && len(assignments) > 0 {
		return
	}

	assignment := &UserRoleAssignment{
		ID:         uuid.New(),
		TenantID:   tenantID,
		UserID:     localUserID,
		RoleID:     role.ID,
		AssignedBy: authUserID,
	}
	if err := s.repo.AssignRoleToUser(ctx, tenantID, assignment); err != nil {
		s.logger.Warn("JIT role assignment failed",
			zap.String("role_code", roleCode),
			zap.Error(err),
		)
		return
	}

	s.logger.Info("JIT assigned marketflow role",
		zap.String("role_code", roleCode),
		zap.String("user_id", localUserID.String()),
		zap.String("tenant_id", tenantID.String()),
	)
}

// mapGlobalRoleToMarketflowRole maps global SSO roles to marketflow service roles.
func mapGlobalRoleToMarketflowRole(roles []string) string {
	for _, r := range roles {
		switch r {
		case "superuser", "admin":
			return "marketflow_admin"
		case "staff", "member":
			return "fbo_owner"
		}
	}
	if len(roles) > 0 {
		return "viewer"
	}
	return "viewer"
}

// SyncUser syncs a user from auth-service.
func (s *Service) SyncUser(ctx context.Context, tenantID uuid.UUID, authServiceUserID uuid.UUID, email string) (*MarketflowUser, error) {
	user, err := s.repo.GetUserByAuthServiceID(ctx, tenantID, authServiceUserID)
	if err == nil {
		updates := &UserUpdates{
			SyncStatus: stringPtr("synced"),
		}
		if err := s.repo.UpdateUser(ctx, tenantID, user.ID, updates); err != nil {
			s.logger.Warn("failed to update user sync status", zap.Error(err))
		}
		return user, nil
	}

	user = &MarketflowUser{
		ID:                authServiceUserID,
		TenantID:          tenantID,
		AuthServiceUserID: authServiceUserID,
		Email:             email,
		Status:            "active",
		SyncStatus:        "synced",
	}

	if err := s.repo.CreateUser(ctx, tenantID, user); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	s.logger.Info("user synced",
		zap.String("tenant_id", tenantID.String()),
		zap.String("user_id", user.ID.String()),
		zap.String("email", email),
	)

	return user, nil
}

// HasPermission checks if a user has a specific permission.
func (s *Service) HasPermission(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, permissionCode string) (bool, error) {
	permissions, err := s.repo.GetUserPermissions(ctx, tenantID, userID)
	if err != nil {
		return false, fmt.Errorf("get user permissions: %w", err)
	}

	for _, perm := range permissions {
		if perm.PermissionCode == permissionCode {
			return true, nil
		}
	}

	return false, nil
}

// HasAnyPermission checks if a user has any of the given permissions.
func (s *Service) HasAnyPermission(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, permissionCodes ...string) (bool, error) {
	permissions, err := s.repo.GetUserPermissions(ctx, tenantID, userID)
	if err != nil {
		return false, fmt.Errorf("get user permissions: %w", err)
	}

	codeSet := make(map[string]bool, len(permissionCodes))
	for _, code := range permissionCodes {
		codeSet[code] = true
	}

	for _, perm := range permissions {
		if codeSet[perm.PermissionCode] {
			return true, nil
		}
	}

	return false, nil
}

// HasRole checks if a user has a specific role.
func (s *Service) HasRole(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, roleCode string) (bool, error) {
	roles, err := s.repo.GetUserRoles(ctx, tenantID, userID)
	if err != nil {
		return false, fmt.Errorf("get user roles: %w", err)
	}

	for _, role := range roles {
		if role.RoleCode == roleCode {
			return true, nil
		}
	}

	return false, nil
}

// ListUsers returns all active users for the given tenant with their roles.
func (s *Service) ListUsers(ctx context.Context, tenantID uuid.UUID) ([]*MarketflowUser, error) {
	return s.repo.ListUsers(ctx, tenantID)
}

// GetUserRoles returns roles for a specific user.
func (s *Service) GetUserRoles(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*MarketflowRole, error) {
	return s.repo.GetUserRoles(ctx, tenantID, userID)
}

// UpdateUserRole replaces a user's current role with a new role code.
func (s *Service) UpdateUserRole(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, newRoleCode string, assignedBy uuid.UUID) error {
	newRole, err := s.repo.GetRoleByCode(ctx, tenantID, newRoleCode)
	if err != nil {
		return fmt.Errorf("role %q not found: %w", newRoleCode, err)
	}
	existing, err := s.repo.GetUserRoles(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	for _, r := range existing {
		_ = s.repo.RevokeRoleFromUser(ctx, tenantID, userID, r.ID)
	}
	return s.repo.AssignRoleToUser(ctx, tenantID, &UserRoleAssignment{
		ID:         uuid.New(),
		TenantID:   tenantID,
		UserID:     userID,
		RoleID:     newRole.ID,
		AssignedBy: assignedBy,
	})
}

// RoleTenantAdmin is the MarketFlow role that may manage the team.
const RoleTenantAdmin = "marketflow_admin"

// AssignableRoles are the system roles a tenant admin can give a member.
var AssignableRoles = []string{RoleTenantAdmin, "fbo_owner", "viewer"}

// UserByAuthID returns the tenant's local user for an SSO user id, or an error when there is none.
func (s *Service) UserByAuthID(ctx context.Context, tenantID, authUserID uuid.UUID) (*MarketflowUser, error) {
	return s.repo.GetUserByAuthServiceID(ctx, tenantID, authUserID)
}

// DeactivateUser marks a tenant user as deactivated.
func (s *Service) DeactivateUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) error {
	return s.repo.DeactivateUser(ctx, tenantID, userID)
}

func stringPtr(s string) *string {
	return &s
}
