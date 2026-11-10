package rbac

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ssoRoles are MarketFlow's own SSO roles (scope marketflow). Invites grant them instead of the
// organisation-wide admin; the caller sync maps them back to MarketFlow roles (audit MF #3).
var ssoRoles = []struct{ code, name, desc string }{
	{"marketflow_admin", "MarketFlow admin", "MarketFlow settings, integrations, team, autopilot and lead export"},
	{"marketflow_member", "MarketFlow member", "Day-to-day marketing in MarketFlow: create, approve, boost, post, reply"},
}

// PushRolesToAuth registers MarketFlow's SSO roles in auth-api's role registry
// (POST /api/v1/s2s/roles/sync, idempotent by role code).
func PushRolesToAuth(ctx context.Context, authURL, serviceKey string) error {
	if serviceKey == "" {
		return fmt.Errorf("rbac: INTERNAL_SERVICE_KEY not set; role registry push skipped")
	}
	roles := make([]map[string]string, 0, len(ssoRoles))
	for _, r := range ssoRoles {
		roles = append(roles, map[string]string{"role_code": r.code, "name": r.name, "description": r.desc, "scope": "marketflow"})
	}
	body, _ := json.Marshal(map[string]any{"service": "marketflow", "roles": roles})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(authURL, "/")+"/api/v1/s2s/roles/sync", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", serviceKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("rbac: role registry push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("rbac: role registry push: HTTP %d", resp.StatusCode)
	}
	return nil
}
