package knowledge

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The shipped file is the platform knowledge base's one source; it must load.
func TestShippedPlatformKBLoads(t *testing.T) {
	entries, err := PlatformEntries()
	if err != nil {
		t.Fatalf("platform_kb.json: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("platform_kb.json is empty")
	}
	for _, e := range entries {
		if strings.Contains(e.Content, "4,999") || strings.Contains(e.Content, "14,999") {
			t.Fatalf("%q still carries the old Starter/Growth pricing", e.Title)
		}
	}
}

func TestParsePlatformKBRejects(t *testing.T) {
	cases := []struct {
		name, json, want string
	}{
		{"empty", `[]`, "empty"},
		{"not json", `{`, "parse"},
		{"tenant scope", `[{"scope":"tenant","category":"c","title":"t","content":"x"}]`, "scope must be platform"},
		{"no content", `[{"scope":"platform","category":"c","title":"t","content":" "}]`, "required"},
		{"duplicate key", `[{"scope":"platform","category":"c","title":"t","content":"x"},{"scope":"platform","category":"c","title":"t","content":"y"}]`, "duplicate"},
		{"reserved metadata", `[{"scope":"platform","category":"c","title":"t","content":"x","metadata":{"stale":true}}]`, "reserved"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parsePlatformKB([]byte(c.json))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// What counts as a difference: anything the file says, plus the marker and the stale flag; the
// seeded_at timestamp does not.
func TestPlatformEntryChanged(t *testing.T) {
	tid := uuid.New()
	e := PlatformEntry{Scope: "platform", Category: "pricing_plans", Title: "Plans", Content: "Starter 2,500.",
		Tags: []string{"pricing"}, Metadata: map[string]any{"source": "plans.ts", "checked": "2026-11-17"}}
	code, cc := "015", 0.102
	e.ProductCode, e.ProductCC = &code, &cc
	want, err := e.desiredMeta(SourceKindPlatformSeed)
	if err != nil {
		t.Fatal(err)
	}
	same := func() platformRow {
		rc, rcc := "015", 0.102
		return platformRow{TenantID: tid, Scope: "platform", Category: e.Category, Title: e.Title, Content: e.Content,
			Tags: []string{"pricing"}, ProductCode: &rc, ProductCC: &rcc,
			Metadata: map[string]any{metaSourceKind: SourceKindPlatformSeed, metaSeedSource: "plans.ts",
				"checked": "2026-11-17", metaSeededAt: "2026-11-17T08:00:00Z"}}
	}
	cases := []struct {
		name   string
		mutate func(r *platformRow)
		want   bool
	}{
		{"identical apart from seeded_at", func(r *platformRow) { r.Metadata[metaSeededAt] = "2026-12-01T08:00:00Z" }, false},
		{"content", func(r *platformRow) { r.Content = "Starter 4,999." }, true},
		{"title", func(r *platformRow) { r.Title = "Pricing" }, true},
		{"tags", func(r *platformRow) { r.Tags = []string{"pricing", "plans"} }, true},
		{"metadata value", func(r *platformRow) { r.Metadata["checked"] = "2026-01-01" }, true},
		{"marker missing", func(r *platformRow) { delete(r.Metadata, metaSourceKind) }, true},
		{"stale", func(r *platformRow) { r.Metadata[metaStale] = true }, true},
		{"scope", func(r *platformRow) { r.Scope = "tenant" }, true},
		{"anchor tenant", func(r *platformRow) { r.TenantID = uuid.New() }, true},
		{"product code", func(r *platformRow) { c := "034"; r.ProductCode = &c }, true},
		{"product code missing", func(r *platformRow) { r.ProductCode = nil }, true},
		{"case credits", func(r *platformRow) { c := 0.146; r.ProductCC = &c }, true},
		{"another set's marker", func(r *platformRow) { r.Metadata[metaSourceKind] = SourceKindKenyaTaxSeed }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := same()
			c.mutate(&r)
			if got := platformEntryChanged(r, "platform", tid, e, want); got != c.want {
				t.Fatalf("changed = %v, want %v", got, c.want)
			}
		})
	}
}

// Every shipped seed set loads, has its own marker, and every row its old create-only seed made
// is named in its legacy list and now produced by its file (the content moved, nothing lost).
func TestShippedSeedSets(t *testing.T) {
	kinds := map[string]bool{}
	for _, set := range SeedSets() {
		t.Run(set.Name, func(t *testing.T) {
			if set.SourceKind == "" || kinds[set.SourceKind] {
				t.Fatalf("source kind %q is empty or shared", set.SourceKind)
			}
			kinds[set.SourceKind] = true
			entries, err := set.Entries()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if set.PlatformOnly != (set.SourceKind == SourceKindPlatformSeed) {
				t.Fatal("only the Codevertex set is platform only; the others keep the run's scope")
			}
			if set.SourceKind == SourceKindPlatformSeed {
				return // its legacy titles were superseded, not moved
			}
			titles, codes := map[string]bool{}, map[string]bool{}
			for _, e := range entries {
				if e.Scope != "" {
					t.Fatalf("%q names a scope; the run decides it", e.Title)
				}
				titles[e.Category+"|"+e.Title] = true
				if e.ProductCode != nil {
					codes[e.Category+"|"+*e.ProductCode] = true
				}
			}
			if len(set.Legacy) != len(entries) {
				t.Fatalf("%d legacy rows for %d entries", len(set.Legacy), len(entries))
			}
			for _, lr := range set.Legacy {
				if lr.ProductCode != "" && !codes[lr.Category+"|"+lr.ProductCode] {
					t.Fatalf("legacy product %s is not in the file", lr.ProductCode)
				}
				if lr.ProductCode == "" && !titles[lr.Category+"|"+lr.Title] {
					t.Fatalf("legacy row %q / %q is not in the file", lr.Category, lr.Title)
				}
			}
		})
	}
}

// The FLP sets build each entry from the catalogue files exactly as the old seed did.
func TestFLPSeedContent(t *testing.T) {
	products, err := FLPProductEntries()
	if err != nil {
		t.Fatal(err)
	}
	gel := products[0]
	if gel.Category != "product" || gel.Title != "Forever Aloe Vera Gel" || *gel.ProductCode != "015" || *gel.ProductCC != 0.102 {
		t.Fatalf("product: %+v", gel)
	}
	if !strings.HasPrefix(gel.Content, "As close to the real thing") ||
		!strings.Contains(gel.Content, "\n\nBenefits:\n- Supports good digestive health\n") ||
		!strings.Contains(gel.Content, "\nContents: 1 ltr\nDirections: Shake well.") {
		t.Fatalf("product content: %q", gel.Content)
	}
	if gel.Metadata[metaSource] == nil || gel.Metadata["last_updated"] != "2026-04-09" {
		t.Fatalf("product metadata: %v", gel.Metadata)
	}

	faq, err := FLPFAQEntries()
	if err != nil {
		t.Fatal(err)
	}
	if faq[0].Category != "faq" || faq[0].Title != "What is Forever Living Products?" {
		t.Fatalf("faq: %+v", faq[0])
	}

	biz, err := FLPBusinessEntries()
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]PlatformEntry{}
	for _, e := range biz {
		byTitle[e.Title] = e
	}
	if c := byTitle["Company Overview"].Content; !strings.HasPrefix(c, "Forever Living Products International was founded in 1978 and operates in 160+ countries with 46+ years of uninterrupted growth.") ||
		!strings.HasSuffix(c, "Certifications: IASC (International Aloe Science Council), USDA Organic (DR farms), EU Organic (DR farms)") {
		t.Fatalf("company overview: %q", c)
	}
	if c := byTitle["FBO Rank Ladder"].Content; !strings.HasPrefix(c, "- Preferred Customer: 0.465 CC (illustration: N/A)\n- Assistant Supervisor: 2 CC (illustration: USD $337-$463)\n") ||
		!strings.Contains(c, "- Senior Manager: 2 personally-sponsored Managers (illustration: USD $5,000)\n") {
		t.Fatalf("rank ladder: %q", c)
	}
	if c := byTitle["How to Get Started"].Content; c != "Step 1: Fill the FBO Form\nStep 2: Obtain your Forever Business ID\nStep 3: Book a planning meeting\n" {
		t.Fatalf("get started: %q", c)
	}
	if c := byTitle["Income Stream: Retail Business"].Content; !strings.Contains(c, "\n\nExample: Retailing USD $960/month") {
		t.Fatalf("income stream: %q", c)
	}

	comp, err := FLPComplianceEntries()
	if err != nil {
		t.Fatal(err)
	}
	if c := comp[0].Content; !strings.HasPrefix(c, "[income]: Profits and bonuses") || !strings.Contains(c, "\n\n[nutritional]: ") {
		t.Fatalf("disclaimers are not in key order: %q", c)
	}
	if c := comp[1].Content; !strings.HasPrefix(c, "- Forever has operated for over 46 years\n") {
		t.Fatalf("facts: %q", c)
	}
}

// A platform entry's id ignores the anchor tenant and keeps the Codevertex ids; a tenant entry's
// id is per tenant; two sets never share an id.
func TestSeedEntryID(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	if PlatformEntryID("c", "t") != uuid.NewSHA1(syncNamespace, []byte("platform_seed|c|t")) {
		t.Fatal("the Codevertex ids must not change")
	}
	if SeedEntryID(SourceKindKenyaTaxSeed, ScopePlatform, a, "c", "t") != SeedEntryID(SourceKindKenyaTaxSeed, ScopePlatform, b, "c", "t") {
		t.Fatal("a platform id does not depend on the anchor tenant")
	}
	if SeedEntryID(SourceKindKenyaTaxSeed, ScopeTenant, a, "c", "t") == SeedEntryID(SourceKindKenyaTaxSeed, ScopeTenant, b, "c", "t") {
		t.Fatal("a tenant id is per tenant")
	}
	if SeedEntryID(SourceKindKenyaTaxSeed, ScopePlatform, a, "c", "t") == SeedEntryID(SourceKindFLPFAQSeed, ScopePlatform, a, "c", "t") {
		t.Fatal("two sets never share an id")
	}
	if SeedEntryID(SourceKindKenyaTaxSeed, ScopePlatform, a, "c", "t") == SeedEntryID(SourceKindKenyaTaxSeed, ScopeTenant, a, "c", "t") {
		t.Fatal("platform and tenant copies never share an id")
	}
}

func TestParseSeedEntriesLeavesScopeToTheRun(t *testing.T) {
	if _, err := parseSeedEntries("x", []byte(`[{"category":"c","title":"t","content":"x"}]`), ""); err != nil {
		t.Fatalf("no scope: %v", err)
	}
	_, err := parseSeedEntries("x", []byte(`[{"scope":"platform","category":"c","title":"t","content":"x"}]`), "")
	if err == nil || !strings.Contains(err.Error(), "seed run") {
		t.Fatalf("got %v, want a scope error", err)
	}
}
