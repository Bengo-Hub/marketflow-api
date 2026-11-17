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
	want, err := e.desiredMeta()
	if err != nil {
		t.Fatal(err)
	}
	same := func() platformRow {
		return platformRow{TenantID: tid, Scope: "platform", Category: e.Category, Title: e.Title, Content: e.Content,
			Tags: []string{"pricing"}, Metadata: map[string]any{metaSourceKind: SourceKindPlatformSeed, metaSeedSource: "plans.ts",
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := same()
			c.mutate(&r)
			if got := platformEntryChanged(r, tid, e, want); got != c.want {
				t.Fatalf("changed = %v, want %v", got, c.want)
			}
		})
	}
}
