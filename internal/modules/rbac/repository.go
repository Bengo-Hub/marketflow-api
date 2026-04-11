package rbac

import (
	"context"

	"github.com/google/uuid"
)

// Repository abstracts persistence for RBAC entities.
type Repository interface {
	// User operations
	CreateUser(ctx context.Context, tenantID uuid.UUID, user *MarketflowUser) error
	GetUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) (*MarketflowUser, error)
	GetUserByAuthServiceID(ctx context.Context, tenantID uuid.UUID, authServiceUserID uuid.UUID) (*MarketflowUser, error)
	UpdateUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, updates *UserUpdates) error

	// Role operations
	CreateRole(ctx context.Context, tenantID uuid.UUID, role *MarketflowRole) error
	GetRole(ctx context.Context, tenantID uuid.UUID, roleID uuid.UUID) (*MarketflowRole, error)
	GetRoleByCode(ctx context.Context, tenantID uuid.UUID, roleCode string) (*MarketflowRole, error)
	ListRoles(ctx context.Context, tenantID uuid.UUID) ([]*MarketflowRole, error)

	// Permission operations
	CreatePermission(ctx context.Context, permission *MarketflowPermission) error
	GetPermission(ctx context.Context, permissionID uuid.UUID) (*MarketflowPermission, error)
	GetPermissionByCode(ctx context.Context, permissionCode string) (*MarketflowPermission, error)
	ListPermissions(ctx context.Context, filters PermissionFilters) ([]*MarketflowPermission, error)

	// Role-Permission operations
	AssignPermissionToRole(ctx context.Context, roleID uuid.UUID, permissionID uuid.UUID) error
	RemovePermissionFromRole(ctx context.Context, roleID uuid.UUID, permissionID uuid.UUID) error
	GetRolePermissions(ctx context.Context, roleID uuid.UUID) ([]*MarketflowPermission, error)

	// User-Role assignment operations
	AssignRoleToUser(ctx context.Context, tenantID uuid.UUID, assignment *UserRoleAssignment) error
	RevokeRoleFromUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, roleID uuid.UUID) error
	GetUserRoles(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*MarketflowRole, error)
	GetUserPermissions(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*MarketflowPermission, error)
	ListUserAssignments(ctx context.Context, tenantID uuid.UUID, filters AssignmentFilters) ([]*UserRoleAssignment, error)
}
