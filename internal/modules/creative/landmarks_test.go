package creative

import (
	"strings"
	"testing"
)

func TestAreaForMatchesTheAddressAsSSOGivesIt(t *testing.T) {
	a := areaFor("", "Pioneer House, Suite 203A, Oginga-Odinga Road, Kisumu CBD, Kenya", "")
	if a == nil || a.Name != "kisumu cbd" || len(a.Landmarks) == 0 || !strings.Contains(a.Landmarks[0], "Mega Plaza") || a.StreetLife == "" {
		t.Fatalf("area %+v", a)
	}
	// The town alone maps to its alias.
	if a := areaFor("", "Milimani", "Kisumu"); a == nil || a.Name != "kisumu cbd" {
		t.Fatalf("alias %+v", a)
	}
	if a := areaFor("", "Reinsurance Plaza, Taifa Road, Nairobi CBD", ""); a == nil || a.Name != "nairobi cbd" || !strings.Contains(strings.Join(a.Landmarks, " "), "KICC") {
		t.Fatalf("nairobi %+v", a)
	}
	// No whole-word match: "Kisumuville" is not Kisumu.
	if a := areaFor("", "Kisumuville Road, Eldoret", ""); a != nil {
		t.Fatalf("a partial word matched %+v", a)
	}
}

// The platform's or tenant's registry replaces the seed; an unreadable one falls back to it.
func TestAreaForReadsTheConfiguredRegistry(t *testing.T) {
	reg := `{"eldoret": {"landmarks": ["the Eldoret Sports Club grounds"], "street_life": "boda bodas"}}`
	if a := areaFor(reg, "Zion Mall, Uganda Road", "Eldoret"); a == nil || a.Name != "eldoret" || a.Landmarks[0] != "the Eldoret Sports Club grounds" {
		t.Fatalf("configured %+v", a)
	}
	if a := areaFor(reg, "Pioneer House", "Kisumu"); a != nil {
		t.Fatalf("a configured registry replaces the seed, got %+v", a)
	}
	if a := areaFor("{not json", "Pioneer House", "Kisumu"); a == nil || a.Name != "kisumu cbd" {
		t.Fatalf("broken registry should fall back to the seed, got %+v", a)
	}
}
