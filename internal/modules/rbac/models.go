package rbac

import (
	"time"

	"github.com/google/uuid"
)

// MarketflowUser represents a marketflow service user reference.
type MarketflowUser struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	AuthServiceUserID uuid.UUID
	Email             string
	Status            string
	SyncStatus        string
	LastSyncAt        *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// MarketflowRole represents a marketflow service role.
type MarketflowRole struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	RoleCode     string
	Name         string
	Description  *string
	IsSystemRole bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// MarketflowPermission represents a marketflow service permission.
type MarketflowPermission struct {
	ID             uuid.UUID
	PermissionCode string
	Name           string
	Module         string
	Action         string
	Resource       *string
	Description    *string
	CreatedAt      time.Time
}

// UserRoleAssignment represents a user role assignment.
type UserRoleAssignment struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	UserID     uuid.UUID
	RoleID     uuid.UUID
	AssignedBy uuid.UUID
	AssignedAt time.Time
	ExpiresAt  *time.Time
}

// UserUpdates for partial user updates.
type UserUpdates struct {
	Status     *string
	SyncStatus *string
}

// PermissionFilters for listing permissions.
type PermissionFilters struct {
	Module *string
	Action *string
}

// AssignmentFilters for listing role assignments.
type AssignmentFilters struct {
	UserID *uuid.UUID
	RoleID *uuid.UUID
}
