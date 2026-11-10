package rbac

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// MF #3: MarketFlow registers its own SSO roles (scope marketflow) so invites can grant them and
// the accounts site lists them, like hadia-api does for its roles.
func TestPushRolesToAuth(t *testing.T) {
	var got struct {
		Service string `json:"service"`
		Roles   []struct {
			RoleCode string `json:"role_code"`
			Scope    string `json:"scope"`
		} `json:"roles"`
	}
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/s2s/roles/sync" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		key = r.Header.Get("X-API-Key")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"synced":true,"count":2}`))
	}))
	defer srv.Close()

	if err := PushRolesToAuth(context.Background(), srv.URL, "FAKE-KEY"); err != nil {
		t.Fatal(err)
	}
	if key != "FAKE-KEY" || got.Service != "marketflow" || len(got.Roles) != 2 {
		t.Fatalf("key %q payload %+v", key, got)
	}
	codes := map[string]string{}
	for _, r := range got.Roles {
		codes[r.RoleCode] = r.Scope
	}
	if codes["marketflow_admin"] != "marketflow" || codes["marketflow_member"] != "marketflow" {
		t.Fatalf("roles %v", codes)
	}
	if err := PushRolesToAuth(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("no key, no push")
	}
}
