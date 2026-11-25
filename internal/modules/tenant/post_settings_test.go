package tenant

import "testing"

func TestValidatePostSettings(t *testing.T) {
	PlatformHosts, PlatformTenantSlug = []string{"app.marketflow.example"}, "platform"
	t.Cleanup(func() { PlatformHosts, PlatformTenantSlug = nil, "" })
	for _, bad := range []any{"http://shop.example/privacy", "shop.example", 5, "https://app.marketflow.example/legal/privacy"} {
		if ValidatePostSettings(map[string]any{PrivacyURLKey: bad}, "shop") == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	ok := map[string]any{PrivacyURLKey: " https://shop.example/privacy ", PostBestTimeKey: "off"}
	if err := ValidatePostSettings(ok, "shop"); err != nil || ok[PrivacyURLKey] != "https://shop.example/privacy" {
		t.Fatalf("%v %v", err, ok)
	}
	own := map[string]any{PrivacyURLKey: "https://app.marketflow.example/legal/privacy"}
	if err := ValidatePostSettings(own, "platform"); err != nil {
		t.Fatal(err)
	}
	clear := map[string]any{PrivacyURLKey: "", PostBestTimeKey: nil}
	if err := ValidatePostSettings(clear, "shop"); err != nil || clear[PrivacyURLKey] != nil {
		t.Fatalf("%v %v", err, clear)
	}
	if ValidatePostSettings(map[string]any{PostBestTimeKey: "maybe"}, "shop") == nil {
		t.Fatal("bad best time accepted")
	}
}
