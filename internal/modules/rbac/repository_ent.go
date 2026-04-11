package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowpermission"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowrole"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/bengobox/marketflow-api/internal/ent/rolepermission"
	"github.com/bengobox/marketflow-api/internal/ent/userroleassignment"
	"github.com/google/uuid"
)

// EntRepository implements the Repository interface using Ent ORM.
type EntRepository struct {
	client *ent.Client
}

// NewEntRepository creates a new Ent-backed repository.
func NewEntRepository(client *ent.Client) *EntRepository {
	return &EntRepository{client: client}
}

func (r *EntRepository) CreateUser(ctx context.Context, tenantID uuid.UUID, user *MarketflowUser) error {
	if user == nil {
		return errors.New("user cannot be nil")
	}
	builder := r.client.MarketflowUser.Create().
		SetID(user.ID).
		SetTenantID(tenantID).
		SetAuthServiceUserID(user.AuthServiceUserID).
		SetEmail(user.Email).
		SetStatus(user.Status).
		SetSyncStatus(user.SyncStatus)
	if user.LastSyncAt != nil {
		builder.SetLastSyncAt(*user.LastSyncAt)
	}
	_, err := builder.Save(ctx)
	return err
}

func (r *EntRepository) GetUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) (*MarketflowUser, error) {
	u, err := r.client.MarketflowUser.Query().
		Where(marketflowuser.ID(userID), marketflowuser.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return mapEntUser(u), nil
}

func (r *EntRepository) GetUserByAuthServiceID(ctx context.Context, tenantID uuid.UUID, authServiceUserID uuid.UUID) (*MarketflowUser, error) {
	u, err := r.client.MarketflowUser.Query().
		Where(marketflowuser.AuthServiceUserID(authServiceUserID), marketflowuser.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user by auth ID: %w", err)
	}
	return mapEntUser(u), nil
}

func (r *EntRepository) UpdateUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, updates *UserUpdates) error {
	builder := r.client.MarketflowUser.Update().
		Where(marketflowuser.ID(userID), marketflowuser.TenantID(tenantID))
	if updates.Status != nil {
		builder.SetStatus(*updates.Status)
	}
	if updates.SyncStatus != nil {
		builder.SetSyncStatus(*updates.SyncStatus)
	}
	_, err := builder.Save(ctx)
	return err
}

func (r *EntRepository) CreateRole(ctx context.Context, tenantID uuid.UUID, role *MarketflowRole) error {
	if role == nil {
		return errors.New("role cannot be nil")
	}
	builder := r.client.MarketflowRole.Create().
		SetID(role.ID).
		SetTenantID(tenantID).
		SetRoleCode(role.RoleCode).
		SetName(role.Name).
		SetIsSystemRole(role.IsSystemRole)
	if role.Description != nil {
		builder.SetDescription(*role.Description)
	}
	_, err := builder.Save(ctx)
	return err
}

func (r *EntRepository) GetRole(ctx context.Context, tenantID uuid.UUID, roleID uuid.UUID) (*MarketflowRole, error) {
	role, err := r.client.MarketflowRole.Query().
		Where(marketflowrole.ID(roleID), marketflowrole.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get role: %w", err)
	}
	return mapEntRole(role), nil
}

func (r *EntRepository) GetRoleByCode(ctx context.Context, tenantID uuid.UUID, roleCode string) (*MarketflowRole, error) {
	role, err := r.client.MarketflowRole.Query().
		Where(marketflowrole.TenantID(tenantID), marketflowrole.RoleCodeEQ(roleCode)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get role by code: %w", err)
	}
	return mapEntRole(role), nil
}

func (r *EntRepository) ListRoles(ctx context.Context, tenantID uuid.UUID) ([]*MarketflowRole, error) {
	roles, err := r.client.MarketflowRole.Query().
		Where(marketflowrole.TenantID(tenantID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	result := make([]*MarketflowRole, len(roles))
	for i, role := range roles {
		result[i] = mapEntRole(role)
	}
	return result, nil
}

func (r *EntRepository) CreatePermission(ctx context.Context, perm *MarketflowPermission) error {
	if perm == nil {
		return errors.New("permission cannot be nil")
	}
	builder := r.client.MarketflowPermission.Create().
		SetID(perm.ID).
		SetPermissionCode(perm.PermissionCode).
		SetName(perm.Name).
		SetModule(perm.Module).
		SetAction(perm.Action)
	if perm.Resource != nil {
		builder.SetResource(*perm.Resource)
	}
	if perm.Description != nil {
		builder.SetDescription(*perm.Description)
	}
	_, err := builder.Save(ctx)
	return err
}

func (r *EntRepository) GetPermission(ctx context.Context, permissionID uuid.UUID) (*MarketflowPermission, error) {
	p, err := r.client.MarketflowPermission.Get(ctx, permissionID)
	if err != nil {
		return nil, fmt.Errorf("get permission: %w", err)
	}
	return mapEntPermission(p), nil
}

func (r *EntRepository) GetPermissionByCode(ctx context.Context, code string) (*MarketflowPermission, error) {
	p, err := r.client.MarketflowPermission.Query().
		Where(marketflowpermission.PermissionCodeEQ(code)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get permission by code: %w", err)
	}
	return mapEntPermission(p), nil
}

func (r *EntRepository) ListPermissions(ctx context.Context, filters PermissionFilters) ([]*MarketflowPermission, error) {
	query := r.client.MarketflowPermission.Query()
	if filters.Module != nil {
		query = query.Where(marketflowpermission.ModuleEQ(*filters.Module))
	}
	if filters.Action != nil {
		query = query.Where(marketflowpermission.ActionEQ(*filters.Action))
	}
	perms, err := query.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list permissions: %w", err)
	}
	result := make([]*MarketflowPermission, len(perms))
	for i, p := range perms {
		result[i] = mapEntPermission(p)
	}
	return result, nil
}

func (r *EntRepository) AssignPermissionToRole(ctx context.Context, roleID uuid.UUID, permissionID uuid.UUID) error {
	_, err := r.client.RolePermission.Create().
		SetRoleID(roleID).
		SetPermissionID(permissionID).
		Save(ctx)
	return err
}

func (r *EntRepository) RemovePermissionFromRole(ctx context.Context, roleID uuid.UUID, permissionID uuid.UUID) error {
	_, err := r.client.RolePermission.Delete().
		Where(
			rolepermission.RoleID(roleID),
			rolepermission.PermissionID(permissionID),
		).
		Exec(ctx)
	return err
}

func (r *EntRepository) GetRolePermissions(ctx context.Context, roleID uuid.UUID) ([]*MarketflowPermission, error) {
	role, err := r.client.MarketflowRole.Query().
		Where(marketflowrole.ID(roleID)).
		WithPermissions().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get role permissions: %w", err)
	}
	result := make([]*MarketflowPermission, len(role.Edges.Permissions))
	for i, p := range role.Edges.Permissions {
		result[i] = mapEntPermission(p)
	}
	return result, nil
}

func (r *EntRepository) AssignRoleToUser(ctx context.Context, tenantID uuid.UUID, assignment *UserRoleAssignment) error {
	if assignment == nil {
		return errors.New("assignment cannot be nil")
	}
	_, err := r.client.UserRoleAssignment.Create().
		SetID(assignment.ID).
		SetTenantID(tenantID).
		SetUserID(assignment.UserID).
		SetRoleID(assignment.RoleID).
		SetAssignedBy(assignment.AssignedBy).
		Save(ctx)
	return err
}

func (r *EntRepository) RevokeRoleFromUser(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, roleID uuid.UUID) error {
	_, err := r.client.UserRoleAssignment.Delete().
		Where(
			userroleassignment.TenantID(tenantID),
			userroleassignment.UserID(userID),
			userroleassignment.RoleID(roleID),
		).
		Exec(ctx)
	return err
}

func (r *EntRepository) GetUserRoles(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*MarketflowRole, error) {
	assignments, err := r.client.UserRoleAssignment.Query().
		Where(
			userroleassignment.TenantID(tenantID),
			userroleassignment.UserID(userID),
		).
		WithRole().
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user roles: %w", err)
	}
	result := make([]*MarketflowRole, 0, len(assignments))
	for _, a := range assignments {
		if a.Edges.Role != nil {
			result = append(result, mapEntRole(a.Edges.Role))
		}
	}
	return result, nil
}

func (r *EntRepository) GetUserPermissions(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*MarketflowPermission, error) {
	// Get all roles for user, then get all permissions for those roles
	roles, err := r.GetUserRoles(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}

	permMap := make(map[uuid.UUID]*MarketflowPermission)
	for _, role := range roles {
		perms, err := r.GetRolePermissions(ctx, role.ID)
		if err != nil {
			continue
		}
		for _, p := range perms {
			permMap[p.ID] = p
		}
	}

	result := make([]*MarketflowPermission, 0, len(permMap))
	for _, p := range permMap {
		result = append(result, p)
	}
	return result, nil
}

func (r *EntRepository) ListUserAssignments(ctx context.Context, tenantID uuid.UUID, filters AssignmentFilters) ([]*UserRoleAssignment, error) {
	query := r.client.UserRoleAssignment.Query().
		Where(userroleassignment.TenantID(tenantID))
	if filters.UserID != nil {
		query = query.Where(userroleassignment.UserID(*filters.UserID))
	}
	if filters.RoleID != nil {
		query = query.Where(userroleassignment.RoleID(*filters.RoleID))
	}
	assignments, err := query.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list assignments: %w", err)
	}
	result := make([]*UserRoleAssignment, len(assignments))
	for i, a := range assignments {
		ua := &UserRoleAssignment{
			ID:         a.ID,
			TenantID:   a.TenantID,
			UserID:     a.UserID,
			RoleID:     a.RoleID,
			AssignedBy: a.AssignedBy,
			AssignedAt: a.AssignedAt,
		}
		if !a.ExpiresAt.IsZero() {
			t := a.ExpiresAt
			ua.ExpiresAt = &t
		}
		result[i] = ua
	}
	return result, nil
}

// Mapping functions
func mapEntUser(u *ent.MarketflowUser) *MarketflowUser {
	mu := &MarketflowUser{
		ID:                u.ID,
		TenantID:          u.TenantID,
		AuthServiceUserID: u.AuthServiceUserID,
		Email:             u.Email,
		Status:            u.Status,
		SyncStatus:        u.SyncStatus,
		CreatedAt:         u.CreatedAt,
		UpdatedAt:         u.UpdatedAt,
	}
	if !u.LastSyncAt.IsZero() {
		t := u.LastSyncAt
		mu.LastSyncAt = &t
	}
	return mu
}

func mapEntRole(r *ent.MarketflowRole) *MarketflowRole {
	mr := &MarketflowRole{
		ID:           r.ID,
		TenantID:     r.TenantID,
		RoleCode:     r.RoleCode,
		Name:         r.Name,
		IsSystemRole: r.IsSystemRole,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
	if r.Description != "" {
		mr.Description = &r.Description
	}
	return mr
}

func mapEntPermission(p *ent.MarketflowPermission) *MarketflowPermission {
	mp := &MarketflowPermission{
		ID:             p.ID,
		PermissionCode: p.PermissionCode,
		Name:           p.Name,
		Module:         p.Module,
		Action:         p.Action,
		CreatedAt:      p.CreatedAt,
	}
	if p.Resource != "" {
		mp.Resource = &p.Resource
	}
	if p.Description != "" {
		mp.Description = &p.Description
	}
	return mp
}
