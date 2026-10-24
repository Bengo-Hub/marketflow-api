package tenant

import (
	"testing"

	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
)

func TestPublicMetadataDropsPrivateKeys(t *testing.T) {
	meta := map[string]any{
		"bio":                     "Hello",
		"offerings":               []any{map[string]any{"name": "Gel"}},
		marketingos.MetadataKey:   map[string]any{"audience": "secret"},
		marketingos.DailyBriefKey: false,
		"anything_else":           "private",
	}
	got := PublicMetadata(meta)
	if got["bio"] != "Hello" || got["offerings"] == nil {
		t.Fatalf("public keys missing: %#v", got)
	}
	for _, k := range []string{marketingos.MetadataKey, marketingos.DailyBriefKey, "anything_else"} {
		if _, ok := got[k]; ok {
			t.Fatalf("private key %q leaked", k)
		}
	}
	if len(PublicMetadata(nil)) != 0 {
		t.Fatal("nil metadata should give an empty map")
	}
}

func TestProfileMetadataPatchSkipsReserved(t *testing.T) {
	update := map[string]any{marketingos.MetadataKey: "stale", "bio": "new", marketingos.DailyBriefKey: true, OnboardingMetadataKey: map[string]any{}, "story": nil}
	got := ProfileMetadataPatch(update)
	for _, k := range []string{marketingos.MetadataKey, marketingos.DailyBriefKey, OnboardingMetadataKey} {
		if _, ok := got[k]; ok {
			t.Fatalf("reserved key %q written through the profile", k)
		}
	}
	if got["bio"] != "new" {
		t.Fatal("normal key dropped")
	}
	if v, ok := got["story"]; !ok || v != nil {
		t.Fatal("a null value must pass through so the key is removed")
	}
}

func TestValidateOfferings(t *testing.T) {
	ok := []any{map[string]any{"name": "Gel", "url": "https://shop.example.com/x", "image_url": "/images/a.jpg"}}
	if err := ValidateOfferings(ok); err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	if err := ValidateOfferings(defaultOfferingsValue()); err != nil {
		t.Fatalf("FLP defaults rejected: %v", err)
	}
	bad := map[string]any{
		"not a list":     "x",
		"no name":        []any{map[string]any{"url": "https://a.b"}},
		"javascript url": []any{map[string]any{"name": "x", "url": "javascript:alert(1)"}},
		"protocol rel":   []any{map[string]any{"name": "x", "image_url": "//evil.example/a.png"}},
		"data url":       []any{map[string]any{"name": "x", "image_url": "data:image/png;base64,AAA"}},
	}
	for name, v := range bad {
		if err := ValidateOfferings(v); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	tooMany := make([]any, maxOfferings+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"name": "x"}
	}
	if err := ValidateOfferings(tooMany); err == nil {
		t.Error("expected the item cap to apply")
	}
}

func TestIsFLPTenant(t *testing.T) {
	cases := []struct {
		slug string
		meta map[string]any
		want bool
	}{
		{"254000123456", nil, true},
		{"boi-enterprises", nil, false},
		{"codevertex", map[string]any{}, false},
		{"jane-fbo", map[string]any{"business_type": "FLP"}, true},
		{"1234567", nil, false},
	}
	for _, c := range cases {
		if got := isFLPTenant(c.slug, c.meta); got != c.want {
			t.Errorf("isFLPTenant(%q) = %v, want %v", c.slug, got, c.want)
		}
	}
}

func TestApplyFBODefaultsKeepsExistingValues(t *testing.T) {
	custom := []any{map[string]any{"name": "My own product"}}
	got := applyFBODefaults(map[string]any{"bio": "Mine", "offerings": custom})
	if got["bio"] != "Mine" {
		t.Fatal("existing bio replaced")
	}
	if list, _ := got["offerings"].([]any); len(list) != 1 {
		t.Fatal("existing offerings replaced")
	}
	fresh := applyFBODefaults(nil)
	list, _ := fresh["offerings"].([]any)
	if len(list) != len(DefaultFLPOfferings) {
		t.Fatalf("want %d default products, got %d", len(DefaultFLPOfferings), len(list))
	}
	first, _ := list[0].(map[string]any)
	if first["flp_item_code"] != "015" || first["name"] != "Aloe Vera Gel" {
		t.Fatalf("unexpected first product: %#v", first)
	}
}

func TestMergeMissingLocalWins(t *testing.T) {
	got := mergeMissing(map[string]any{marketingos.MetadataKey: "local"}, map[string]any{marketingos.MetadataKey: "auth", "tagline": "t"})
	if got[marketingos.MetadataKey] != "local" || got["tagline"] != "t" {
		t.Fatalf("unexpected merge: %#v", got)
	}
}
