package worker

import (
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Sprint 18 L43: a finished boost's final spend is recorded once, a day after its end date.
func TestFinalSpendDue(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	ended := now.Add(-48 * time.Hour)
	recent := now.Add(-2 * time.Hour)
	if !finalSpendDue(&ent.Campaign{EndDate: &ended, Metadata: map[string]any{}}, now) {
		t.Error("ended two days ago: due")
	}
	if finalSpendDue(&ent.Campaign{EndDate: &recent, Metadata: map[string]any{}}, now) {
		t.Error("ended two hours ago: wait for Meta's late reporting")
	}
	if finalSpendDue(&ent.Campaign{EndDate: &ended, Metadata: map[string]any{"final_spend": 10.0}}, now) {
		t.Error("recorded once")
	}
	if finalSpendDue(&ent.Campaign{Metadata: map[string]any{}}, now) {
		t.Error("no end date, nothing to record")
	}
}
