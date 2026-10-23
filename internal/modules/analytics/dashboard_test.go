package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"go.uber.org/zap"
)

func TestDashboardDays(t *testing.T) {
	for in, want := range map[string]int{"": 30, "7": 7, "30": 30, "90": 90, "365": 30, "x": 30} {
		if got := dashboardDays(in); got != want {
			t.Errorf("dashboardDays(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestFillDaysCoversWindowOldestFirst(t *testing.T) {
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC) // 12:00 in Nairobi
	got := fillDays([]DayCount{{Day: "2026-10-23", Count: 4}, {Day: "2026-10-21", Count: 2}}, now, 7)
	if len(got) != 7 || got[0].Day != "2026-10-17" || got[6].Day != "2026-10-23" {
		t.Fatalf("unexpected window: %+v", got)
	}
	if got[6].Count != 4 || got[4].Count != 2 || got[5].Count != 0 {
		t.Fatalf("counts not placed on their days: %+v", got)
	}
}

func TestTopNFoldsTheRest(t *testing.T) {
	rows := []SourceCount{{"a", 1}, {"b", 5}, {"c", 3}, {"d", 2}}
	got := topN(rows, 2)
	if len(got) != 3 || got[0].Source != "b" || got[1].Source != "c" || got[2] != (SourceCount{"other", 3}) {
		t.Fatalf("topN = %+v", got)
	}
	if len(topN([]SourceCount{{"a", 1}}, 8)) != 1 {
		t.Fatal("short list should pass through")
	}
}

func TestDashboardLeadFiguresInDatabase(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db)
	t.Cleanup(func() { _, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx) })
	now := time.Now().UTC()
	for _, l := range []struct {
		source, status string
		age            time.Duration
	}{
		{"facebook", "new", time.Hour},
		{"facebook", "converted", 2 * time.Hour},
		{"", "new", 3 * time.Hour},
		{"website", "new", 9 * 24 * time.Hour},
		{"website", "new", 40 * 24 * time.Hour},
	} {
		if _, err := db.Lead.Create().SetTenantID(tid).SetSource(l.source).SetStatus(l.status).
			SetCreatedAt(now.Add(-l.age)).SetUpdatedAt(now).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	data, err := NewHandler(db, zap.NewNop()).dashboard(ctx, tid, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	l := data.Leads
	if l.Total != 5 || l.ThisWeek != 3 || l.PrevWeek != 1 || l.Converted != 1 || l.InWindow != 4 {
		t.Fatalf("lead stats = %+v", l)
	}
	if len(data.Daily) != 30 {
		t.Fatalf("daily has %d points, want 30", len(data.Daily))
	}
	sum := 0
	for _, d := range data.Daily {
		sum += d.Count
	}
	if sum != 4 {
		t.Fatalf("daily series sums to %d, want 4", sum)
	}
	bySource := map[string]int{}
	for _, s := range data.BySource {
		bySource[s.Source] = s.Count
	}
	if bySource["facebook"] != 2 || bySource["unknown"] != 1 || bySource["website"] != 1 {
		t.Fatalf("by source = %+v", data.BySource)
	}
}
