package custom_agents

import (
	"testing"
	"time"
)

func TestNextBriefRotation(t *testing.T) {
	cfg := okConfig()
	cfg.OfferingIDs = []string{"o1", "o2"}
	cfg.Languages = []string{"en", "sw"}
	cfg.VideoTiers = []string{"template"}
	offers := []map[string]any{{"name": "Blender"}, {"name": "Kettle"}}
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

	want := []struct{ name, lang string }{{"Blender", "en"}, {"Kettle", "en"}, {"Blender", "sw"}, {"Kettle", "sw"}, {"Blender", "en"}}
	for n, w := range want {
		b, err := NextBrief("a1", cfg, offers, n, now)
		if err != nil {
			t.Fatal(err)
		}
		if b.Offering["name"] != w.name || b.Language != w.lang || b.Objective != "leads" || b.Metadata["autopilot_run"] != n {
			t.Fatalf("run %d: %+v", n, b)
		}
	}
}

func TestNextBriefRefusesMissingOfferings(t *testing.T) {
	cfg := okConfig()
	cfg.OfferingIDs = []string{"o1", "o2"}
	if _, err := NextBrief("a1", cfg, []map[string]any{{"name": "Blender"}}, 0, time.Now()); err == nil {
		t.Fatal("a missing offering snapshot must fail, not guess")
	}
	if _, err := NextBrief("a1", cfg, []map[string]any{{"name": ""}, {"name": "x"}}, 0, time.Now()); err == nil {
		t.Fatal("an offering with no name must fail")
	}
}

// The month's sums use the most runs a month can hold, and the ceiling caps what fits.
func TestBuildDryRun(t *testing.T) {
	cfg := okConfig()
	cfg.CreditCap = 20
	b := RunBrief{Offering: map[string]any{"name": "Blender"}, Language: "en", Tier: "template"}
	d := BuildDryRun(cfg, b, 1, 9, 50, true)
	if d.Offering != "Blender" || d.RunsPerMonth != 5 || d.MonthlyDraftCredits != 5 || d.MonthlyFullCredits != 50 {
		t.Fatalf("weekly: %+v", d)
	}
	if d.DraftsWithinCeiling != 5 || d.AdsWithinCeiling != 2 {
		t.Fatalf("ceiling: %+v", d)
	}
	cfg.Cadence = "daily"
	if d := BuildDryRun(cfg, b, 1, 9, 50, true); d.RunsPerMonth != 31 || d.DraftsWithinCeiling != 20 {
		t.Fatalf("daily: %+v", d)
	}
	if d := BuildDryRun(cfg, b, 1, 9, 50, false); d.CreditsKnown || d.MonthlyFullCredits != 0 || d.Balance != -1 || d.Offering != "Blender" {
		t.Fatalf("unknown prices keep the preview without sums: %+v", d)
	}
}
