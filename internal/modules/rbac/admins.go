package rbac

import (
	"context"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowrole"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/bengobox/marketflow-api/internal/ent/userroleassignment"
	"github.com/google/uuid"
)

// NoticeRoles are the roles told about account-level events (tokens, posts, files).
var NoticeRoles = []string{"marketflow_admin", "fbo_owner"}

// TenantAdminEmails returns the emails of the tenant's active users holding one of roles
// (deduplicated, at most 20). Shared by the API and the worker jobs that email admins.
func TenantAdminEmails(ctx context.Context, db *ent.Client, tenantID uuid.UUID, roles []string) ([]string, error) {
	assignments, err := db.UserRoleAssignment.Query().
		Where(
			userroleassignment.TenantID(tenantID),
			userroleassignment.HasRoleWith(marketflowrole.RoleCodeIn(roles...)),
			userroleassignment.HasUserWith(marketflowuser.TenantID(tenantID), marketflowuser.Status("active")),
		).
		WithUser().
		Limit(20).
		All(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(assignments))
	for _, a := range assignments {
		if u := a.Edges.User; u != nil && u.Email != "" && !seen[u.Email] {
			seen[u.Email] = true
			out = append(out, u.Email)
		}
	}
	return out, nil
}
