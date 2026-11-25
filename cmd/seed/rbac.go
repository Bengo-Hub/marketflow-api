package main

import (
	"context"
	"crypto/sha256"
	"log"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowpermission"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowrole"
	"github.com/bengobox/marketflow-api/internal/ent/rolepermission"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
)

// The permission catalogue and what each role holds live in internal/modules/rbac/permissions.go,
// beside the write gate that enforces them. The seed copies them to the tables and nothing else.

// permUUID generates a deterministic UUID from a permission code.
func permUUID(code string) uuid.UUID {
	hash := sha256.Sum256([]byte("marketflow:" + code))
	return uuid.UUID(hash[:16])
}

// roleUUID generates a deterministic UUID from tenant + role code.
func roleUUID(tenantID uuid.UUID, roleCode string) uuid.UUID {
	hash := sha256.Sum256([]byte("marketflow_role:" + tenantID.String() + ":" + roleCode))
	return uuid.UUID(hash[:16])
}

// seedPermissions creates the missing permissions (global, not tenant-scoped).
func seedPermissions(ctx context.Context, client *ent.Client) int {
	count := 0
	for _, p := range rbac.Permissions() {
		if exists, _ := client.MarketflowPermission.Query().
			Where(marketflowpermission.PermissionCodeEQ(p.Code)).Exist(ctx); exists {
			continue
		}
		err := client.MarketflowPermission.Create().
			SetID(permUUID(p.Code)).
			SetPermissionCode(p.Code).
			SetName(p.Name).
			SetModule(p.Module).
			SetAction(p.Action).
			SetDescription(p.Description).
			Exec(ctx)
		if err != nil {
			log.Printf("  ! permission %s: %v", p.Code, err)
			continue
		}
		count++
	}
	return count
}

// seedRoles creates a tenant's missing system roles.
func seedRoles(ctx context.Context, client *ent.Client, tenantID uuid.UUID) int {
	count := 0
	for _, r := range rbac.SystemRoles() {
		if exists, _ := client.MarketflowRole.Query().
			Where(marketflowrole.TenantID(tenantID), marketflowrole.RoleCodeEQ(r.Code)).Exist(ctx); exists {
			continue
		}
		err := client.MarketflowRole.Create().
			SetID(roleUUID(tenantID, r.Code)).
			SetTenantID(tenantID).
			SetRoleCode(r.Code).
			SetName(r.Name).
			SetDescription(r.Description).
			SetIsSystemRole(true).
			Exec(ctx)
		if err != nil {
			log.Printf("  ! role %s: %v", r.Code, err)
			continue
		}
		count++
	}
	return count
}

// seedRolePermissionMappings brings the system roles' permissions in line with rbac.RoleGrants:
// missing ones are added, ones a role no longer holds are removed. The roles are found by code,
// since the first sync of a tenant may have created them under another id.
func seedRolePermissionMappings(ctx context.Context, client *ent.Client, tenantID uuid.UUID) int {
	perms, err := client.MarketflowPermission.Query().All(ctx)
	if err != nil {
		log.Printf("  ! permissions: %v", err)
		return 0
	}
	added, removed := 0, 0
	for _, r := range rbac.SystemRoles() {
		role, err := client.MarketflowRole.Query().
			Where(marketflowrole.TenantID(tenantID), marketflowrole.RoleCodeEQ(r.Code)).Only(ctx)
		if err != nil {
			log.Printf("  ! role %s: %v", r.Code, err)
			continue
		}
		rows, err := client.RolePermission.Query().Where(rolepermission.RoleID(role.ID)).All(ctx)
		if err != nil {
			log.Printf("  ! mappings of %s: %v", r.Code, err)
			continue
		}
		have := make(map[uuid.UUID]bool, len(rows))
		for _, row := range rows {
			have[row.PermissionID] = true
		}
		for _, p := range perms {
			switch want := rbac.RoleGrants(r.Code, p.PermissionCode); {
			case want && !have[p.ID]:
				if err := client.RolePermission.Create().SetRoleID(role.ID).SetPermissionID(p.ID).Exec(ctx); err != nil {
					log.Printf("  ! %s to %s: %v", p.PermissionCode, r.Code, err)
					continue
				}
				added++
			case !want && have[p.ID]:
				if _, err := client.RolePermission.Delete().
					Where(rolepermission.RoleID(role.ID), rolepermission.PermissionID(p.ID)).Exec(ctx); err != nil {
					log.Printf("  ! %s from %s: %v", p.PermissionCode, r.Code, err)
					continue
				}
				removed++
			}
		}
	}
	if removed > 0 {
		log.Printf("  removed %d role-permission mappings no role holds any more", removed)
	}
	return added
}
