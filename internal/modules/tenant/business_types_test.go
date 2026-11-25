package tenant

import (
	"github.com/bengobox/marketflow-api/internal/platform/shareddefaults"

	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// The owner's list (2026-11-14) next to flp and generic.
var ownerBusinessTypes = []string{"generic", "flp", "restaurant", "retail", "ecommerce", "services", "saas", "salon", "pharmacy", "hospitality"}

func TestBusinessTypesList(t *testing.T) {
	seen := map[string]bool{}
	for _, bt := range BusinessTypes {
		if seen[bt.Key] {
			t.Fatalf("duplicate key %q", bt.Key)
		}
		seen[bt.Key] = true
		if bt.Key != strings.ToLower(bt.Key) || bt.Label == "" || bt.Description == "" {
			t.Fatalf("%q needs a lower case key, a label and a description", bt.Key)
		}
		for _, c := range shareddefaults.TypeClaims(bt.Key) {
			if len(c) > 300 || strings.Contains(c, "—") {
				t.Fatalf("%q banned claim too long or uses an em dash: %q", bt.Key, c)
			}
		}
	}
	for _, k := range ownerBusinessTypes {
		if !seen[k] {
			t.Fatalf("business type %q is missing", k)
		}
	}
	if len(seen) != len(ownerBusinessTypes) {
		t.Fatalf("unexpected business types: %v", seen)
	}
}

func TestNormalizeBusinessType(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"pharmacy", "pharmacy", true},
		{" Hospitality ", "hospitality", true},
		{"FLP", "flp", true},
		{"generic", "generic", true},
		{"", "", false},
		{"codevertex", "", false},
		{"e-commerce", "", false}, // keys only; the UI sends "ecommerce"
		{"cafe", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeBusinessType(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeBusinessType(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// Forever Living is admin-only (owner, 2026-11-14).
func TestBusinessTypeChangeAllowed(t *testing.T) {
	cases := []struct {
		current, next string
		admin, want   bool
	}{
		{"generic", "pharmacy", false, true},
		{"salon", "retail", false, true},
		{"generic", "flp", false, false},
		{"flp", "generic", false, false},
		{"FLP", "salon", false, false},
		{"flp", "flp", false, true},
		{"generic", "flp", true, true},
		{"flp", "restaurant", true, true},
	}
	for _, c := range cases {
		if got := businessTypeChangeAllowed(c.current, c.next, c.admin); got != c.want {
			t.Errorf("businessTypeChangeAllowed(%q, %q, admin=%v) = %v, want %v", c.current, c.next, c.admin, got, c.want)
		}
	}
	for _, bt := range BusinessTypes {
		if bt.AdminOnly != (bt.Key == BusinessTypeFLP) {
			t.Fatalf("only flp is admin-only, %q has AdminOnly=%v", bt.Key, bt.AdminOnly)
		}
	}
}

func TestListBusinessTypesHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(nil, zap.NewNop()).ListBusinessTypes(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tenant/profile/business-types", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		BusinessTypes []map[string]any `json:"business_types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.BusinessTypes) != len(BusinessTypes) {
		t.Fatalf("got %d types", len(body.BusinessTypes))
	}
	first := body.BusinessTypes[0]
	if first["key"] == "" || first["label"] == "" || first["description"] == "" {
		t.Fatalf("each type needs key, label and description: %v", first)
	}
	if _, leaked := first["BannedClaims"]; leaked {
		t.Fatal("banned claims are internal to the draft")
	}
}

// TestBusinessTypesMatchMarketflowAI compares this mirror with marketflow-ai's
// internal/compliance/data/business_types.yaml, which owns the rules. It runs when a
// marketflow-ai checkout is found (MARKETFLOW_AI_DIR, or a marketflow-ai directory next to this
// repository) and is skipped otherwise, as in CI where only this repository is checked out.
func TestBusinessTypesMatchMarketflowAI(t *testing.T) {
	path := marketflowAITypesFile()
	if path == "" {
		t.Skip("no marketflow-ai checkout; set MARKETFLOW_AI_DIR to compare the business types")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// The file lists each type as "  - key: x" followed by "    label: y".
	ai := map[string]string{}
	var order []string
	key := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "  - key: "); ok {
			key = strings.TrimSpace(v)
			order = append(order, key)
			continue
		}
		if v, ok := strings.CutPrefix(line, "    label: "); ok && key != "" {
			ai[key] = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(order) != len(BusinessTypes) {
		t.Fatalf("marketflow-ai has %d types %v, this mirror has %d", len(order), order, len(BusinessTypes))
	}
	for i, bt := range BusinessTypes {
		if order[i] != bt.Key || ai[bt.Key] != bt.Label {
			t.Fatalf("type %d: marketflow-ai has %q (%q), this mirror has %q (%q)", i+1, order[i], ai[order[i]], bt.Key, bt.Label)
		}
	}
}

func marketflowAITypesFile() string {
	const rel = "internal/compliance/data/business_types.yaml"
	var dirs []string
	if d := os.Getenv("MARKETFLOW_AI_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	// This package sits at internal/modules/tenant; the repository root is three levels up.
	if wd, err := os.Getwd(); err == nil {
		root := filepath.Join(wd, "..", "..", "..")
		dirs = append(dirs, filepath.Join(root, "..", "marketflow-ai"))
	}
	for _, d := range dirs {
		p := filepath.Join(d, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
