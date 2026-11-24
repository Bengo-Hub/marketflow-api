package google

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Google refused the asset queries live (2026-11-25, EXPECTED_REFERENCED_FIELD_IN_SELECT_CLAUSE):
// the fields they filter on must also be selected. The fakes cannot see that, so the text is checked.
func TestAssetQueriesSelectTheFieldsTheyFilterOn(t *testing.T) {
	src, err := os.ReadFile("ads_setup.go")
	if err != nil {
		t.Fatal(err)
	}
	joined := regexp.MustCompile(`"\s*\+\s*"`).ReplaceAllString(string(src), "")
	found := 0
	for _, m := range regexp.MustCompile(`"SELECT ([^"]*) FROM (customer_asset|campaign_asset) WHERE ([^"]*)"`).FindAllStringSubmatch(joined, -1) {
		found++
		for _, field := range regexp.MustCompile(`[a-z_]+\.[a-z_]+`).FindAllString(m[3], -1) {
			if !strings.Contains(m[1], field) {
				t.Errorf("%s filters on %s without selecting it", m[2], field)
			}
		}
	}
	if found != 2 {
		t.Fatalf("checked %d asset queries, want 2", found)
	}
}
