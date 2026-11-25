package shareddefaults

import (
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
)

func with(t *testing.T, forever map[string]any, claims map[string][]string) {
	t.Helper()
	s := &set{forever: codeDefaults(), claims: claims}
	for k, v := range forever {
		s.forever[k] = v
	}
	loaded.Store(s)
	t.Cleanup(func() { loaded.Store(nil) })
}

func fbo(meta map[string]any) *ent.Tenant { return &ent.Tenant{BusinessType: "flp", Metadata: meta} }

// The owner's rule: a stored value equal to the default follows it when it changes.
func TestEqualValueFollowsAChangedDefault(t *testing.T) {
	ten := fbo(map[string]any{"tagline": DefaultFBOTagline, "offerings": offeringsValue(), "logo_url": LegacyFBOLogo})
	newList := []any{map[string]any{"name": "New product"}}
	with(t, map[string]any{"tagline": "New line", "offerings": newList}, nil)
	m := Meta(ten)
	if m["tagline"] != "New line" || len(m["offerings"].([]any)) != 1 {
		t.Fatalf("copies of the old default must follow: %v %v", m["tagline"], m["offerings"])
	}
	if m["logo_url"] != DefaultFBOLogo {
		t.Fatalf("a copy of an earlier release's default follows too: %v", m["logo_url"])
	}
	if m["bio"] != DefaultFBOBio {
		t.Fatal("a key never stored is inherited")
	}
	if ten.Metadata["tagline"] != DefaultFBOTagline {
		t.Fatal("the stored row is not changed")
	}
}

// A different value, and an emptied one, is the business's own.
func TestDifferentValueStays(t *testing.T) {
	with(t, map[string]any{"tagline": "New line"}, nil)
	m := Meta(fbo(map[string]any{"tagline": "Mine", "story": "", "offerings": []any{map[string]any{"name": "Only this"}}}))
	if m["tagline"] != "Mine" || m["story"] != "" || len(m["offerings"].([]any)) != 1 {
		t.Fatalf("%v %v %v", m["tagline"], m["story"], m["offerings"])
	}
	// Nothing Forever reaches another business.
	if m := Meta(&ent.Tenant{BusinessType: "retail", Metadata: map[string]any{}}); len(m) != 0 {
		t.Fatalf("%v", m)
	}
}

func TestStripInheritedStoresNoCopies(t *testing.T) {
	patch := map[string]any{"tagline": DefaultFBOTagline, "bio": "Mine", "video_url": nil, "offerings": offeringsValue()}
	StripInherited(fbo(nil), patch)
	if patch["tagline"] != nil || patch["offerings"] != nil || patch["bio"] != "Mine" || patch["video_url"] != "" {
		t.Fatalf("%v", patch)
	}
}

// The account's logo and colour: a copy of an earlier account logo follows, a different one stays.
func TestAccountBrandIsInherited(t *testing.T) {
	a, changed := Account{}.Next("https://x/one.png", "#111111")
	if !changed {
		t.Fatal("first read")
	}
	a, _ = a.Next("https://x/two.png", "")
	if again, changed := a.Next("https://x/two.png", "#111111"); changed || again.LogoURL != a.LogoURL {
		t.Fatal("the same values change nothing")
	}
	meta := func(logo string) map[string]any {
		return Meta(&ent.Tenant{BusinessType: "retail", Metadata: map[string]any{"logo_url": logo, AccountKey: a}})
	}
	for stored, want := range map[string]string{"": "https://x/two.png", "https://x/one.png": "https://x/two.png", "https://mine/l.png": "https://mine/l.png"} {
		if got := meta(stored)["logo_url"]; got != want {
			t.Errorf("stored %q: %v, want %q", stored, got, want)
		}
	}
	if meta("")["brand_primary_color"] != "#111111" {
		t.Fatal("a colour the account dropped stays")
	}
}

// Compliance never gets weaker: the shared list always applies, plus the business's own.
func TestBannedClaimsUnion(t *testing.T) {
	with(t, nil, map[string][]string{"all": {"New shared rule"}, "pharmacy": {}})
	shared := SharedClaims("pharmacy")
	for _, must := range append(append([]string{}, genericClaims...), typeClaims["pharmacy"]...) {
		if !strings.Contains(strings.Join(shared, "|"), must) {
			t.Errorf("a platform row may only add: %q is gone", must)
		}
	}
	if shared[len(shared)-1] != "New shared rule" {
		t.Fatalf("%v", shared)
	}
	own := OwnClaims("pharmacy", append([]string{"My own rule", "  " + strings.ToUpper(genericClaims[0])}, typeClaims["pharmacy"]...))
	if len(own) != 1 || own[0] != "My own rule" {
		t.Fatalf("stored copies are not the business's own: %v", own)
	}
	if got := Union(shared, []string{"My own rule", genericClaims[0]}); len(got) != len(shared)+1 {
		t.Fatalf("union %v", got)
	}
	// A rule of a type the business left stays, as its own.
	if own := OwnClaims("retail", typeClaims["pharmacy"]); len(own) != len(typeClaims["pharmacy"]) {
		t.Fatalf("%v", own)
	}
}

func TestBadPlatformValueIsIgnored(t *testing.T) {
	for _, v := range []any{"", "  ", []any{}, 3.0, nil} {
		if usable(DefaultFBOTagline, v) {
			t.Errorf("%v accepted", v)
		}
	}
	if usable(offeringsValue(), "text") {
		t.Error("kind must match")
	}
}
