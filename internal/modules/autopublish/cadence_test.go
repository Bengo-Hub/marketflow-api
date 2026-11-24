package autopublish

import (
	"testing"
	"time"
)

// The veto window ends at the first planned post, and the draft's files last past the last one.
func TestPlanBounds(t *testing.T) {
	base := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	plan := map[string]time.Time{"facebook": base.Add(3 * time.Hour), "tiktok": base.Add(6 * time.Hour)}
	if got := earliest(plan, base); !got.Equal(base.Add(3 * time.Hour)) {
		t.Fatalf("earliest = %s", got)
	}
	if got := latest(plan, base); !got.Equal(base.Add(6 * time.Hour)) {
		t.Fatalf("latest = %s", got)
	}
	if !earliest(nil, base).Equal(base) || !latest(nil, base).Equal(base) {
		t.Fatal("no plan keeps the default")
	}
}
