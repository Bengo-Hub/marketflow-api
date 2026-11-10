package worker

import (
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/adsmanager"
)

// Budget optimiser (away decision 23): the boost's launch state is read from campaign metadata,
// and an ad the optimiser paused once is never judged again (an owner who switches it back on
// keeps it on).
func TestOptimiserMetadata(t *testing.T) {
	c := &ent.Campaign{Metadata: map[string]any{
		"launch": map[string]any{"state": map[string]any{
			"stage": "live", "meta_adset_id": "as1", "meta_ad_id": "ad1",
			"extras": []any{map[string]any{"meta_ad_id": "ad2"}},
		}},
		optimiserMetaKey: map[string]any{"paused": []any{map[string]any{"ad_id": "ad2"}}},
	}}
	st, ok := boostLaunchState(c)
	if !ok || st.Stage != adsmanager.StageLive || st.AdSetID != "as1" || len(st.Extras) != 1 || st.Extras[0].AdID != "ad2" {
		t.Fatalf("state: %+v %v", st, ok)
	}
	if done := alreadyPausedByOptimiser(c); !done["ad2"] || done["ad1"] {
		t.Fatalf("paused: %v", done)
	}
	if _, ok := boostLaunchState(&ent.Campaign{Metadata: map[string]any{}}); ok {
		t.Fatal("a campaign without a launch is not a boost")
	}
}
