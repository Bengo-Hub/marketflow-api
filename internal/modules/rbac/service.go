package rbac

import (
	"context"
	"fmt"

	authclient "github.com/Bengo-Hub/shared-auth-client"
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

// EnsureUserFromToken provisions or refreshes the caller's local user and SSO-derived role
// (see SyncCaller) and returns the local user.
func (s *Service) EnsureUserFromToken(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, email string, tenantSlug string, roles ...string) (*MarketflowUser, error) {
	if err := s.SyncCaller(ctx, tenantID, userID, email, roles); err != nil {
		return nil, err
	}
	return s.repo.GetUserByAuthServiceID(ctx, tenantID, userID)
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
		ID:                uuid.New(), // own id: the same SSO person may be in several tenants
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

// CallerHasAnyRole reports whether the signed-in caller holds one of roles in
// the tenant. Platform owners and service callers always pass. A caller whose
// MarketFlow user is not synced yet has no roles.
func (s *Service) CallerHasAnyRole(ctx context.Context, tenantID uuid.UUID, roles ...string) bool {
	claims, ok := authclient.ClaimsFromContext(ctx)
	if !ok || claims == nil {
		return false
	}
	if claims.IsPlatformOwner || claims.IsService {
		return true
	}
	authID, err := claims.UserID()
	if err != nil {
		return false
	}
	u, err := s.repo.GetUserByAuthServiceID(ctx, tenantID, authID)
	if err != nil || u == nil || u.Status == statusDeactivated {
		return false
	}
	have, err := s.repo.GetUserRoles(ctx, tenantID, u.ID)
	if err != nil {
		return false
	}
	for _, h := range have {
		for _, want := range roles {
			if h.RoleCode == want {
				return true
			}
		}
	}
	return false
}

// DeactivateUser marks a tenant user as deactivated.
func (s *Service) DeactivateUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) error {
	return s.repo.DeactivateUser(ctx, tenantID, userID)
}

func stringPtr(s string) *string {
	return &s
}
