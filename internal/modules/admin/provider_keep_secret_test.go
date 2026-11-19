package admin

import (
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Saving a provider with a secret left blank, masked or out of the form keeps the stored secret;
// a new value replaces it; plain keys always follow the form.
func TestKeptSecrets(t *testing.T) {
	stored := []*ent.ProviderSetting{
		{Key: "app_id", Value: "1692"},
		{Key: "app_secret", Value: "enc:abc", IsSecret: true, IsEncrypted: true},
		{Key: "client_secret", Value: "enc:def", IsEncrypted: true},
		{Key: "developer_token", Value: "enc:ghi", IsSecret: true},
		{Key: "refresh_token", Value: "", IsSecret: true},
		{Key: "_config", Value: "configured", IsSecret: true},
	}
	got := keptSecrets(stored, map[string]string{
		"app_id":          "1692",
		"app_secret":      "",
		"client_secret":   "••••••••", // the form's mask, not a credential (secret-scan:allow)
		"developer_token": "new-token",
	})
	keys := map[string]bool{}
	for _, s := range got {
		keys[s.Key] = true
	}
	if !keys["app_secret"] || !keys["client_secret"] || keys["developer_token"] || keys["app_id"] || keys["refresh_token"] || keys["_config"] {
		t.Fatalf("kept: %v", keys)
	}
	if len(keptSecrets(stored, map[string]string{"app_id": "1"})) != 3 {
		t.Fatal("secrets left out of the form are kept too")
	}
}
