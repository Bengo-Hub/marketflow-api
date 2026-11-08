package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A tenant with competitors is scanned once a week and keeps the winners summary; a tenant
// without competitors is never sent.
func TestCompetitorScanWeekly(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	with := seedTenant(t, db, "cscan")
	without := seedTenant(t, db, "cscan-none")
	db.Tenant.UpdateOneID(with).SetMetadata(map[string]any{"competitors": []string{"rival.co.ke"}}).ExecX(ctx)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"results":[{"query":"rival.co.ke","ads":[{"advertiser":"Rival","format":"video","total_days_shown":40,"still_running":true,"score":80}]}]}`))
	}))
	defer srv.Close()
	claimed := map[string]bool{}
	c := NewCompetitorScan(db, srv.URL, "k", zap.NewNop())
	c.claim = func(_ context.Context, key string, _ time.Duration) bool {
		if claimed[key] {
			return false
		}
		claimed[key] = true
		return true
	}
	c.tick(ctx)
	c.tick(ctx) // same week: no second scan
	if calls < 1 {
		t.Fatal("the tenant with competitors must be scanned")
	}
	for k := range claimed {
		if len(k) > 0 && containsID(k, without.String()) {
			t.Fatal("a tenant without competitors is never scanned")
		}
	}
	m := db.Tenant.GetX(ctx, with).Metadata
	scan, _ := m["competitor_scan"].(map[string]any)
	if scan == nil || scan["scanned_at"] == nil || m["competitors"] == nil {
		t.Fatalf("summary kept and competitors untouched: %v", m)
	}
}

// A week cut short by the quota keeps the earlier winners of competitors it could not search, and
// a removed competitor drops out.
func TestMergeScanKeepsEarlierWinners(t *testing.T) {
	now := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	meta := map[string]any{"competitor_scan": map[string]any{"competitors": []any{
		map[string]any{"query": "a.co.ke", "winners": []any{map[string]any{"advertiser": "A old"}}, "read_at": "2026-11-01T06:00:00Z"},
		map[string]any{"query": "b.co.ke", "winners": []any{map[string]any{"advertiser": "B old"}}, "read_at": "2026-11-01T06:00:00Z"},
		map[string]any{"query": "gone.co.ke", "winners": []any{map[string]any{"advertiser": "Gone"}}},
	}}}
	var out scanResult
	if err := json.Unmarshal([]byte(`{"results":[{"query":"A.co.ke","ads":[{"advertiser":"A new"}]},{"query":"c.co.ke","error":"serpapi 500"}],"share_used":true}`), &out); err != nil {
		t.Fatal(err)
	}
	got := mergeScan(meta, []string{"a.co.ke", "b.co.ke", "c.co.ke", "d.co.ke"}, out, now)
	per := got["competitors"].([]map[string]any)
	if len(per) != 4 {
		t.Fatalf("one entry per listed competitor, removed ones dropped: %v", per)
	}
	if w := fmt.Sprint(per[0]["winners"]); !strings.Contains(w, "A new") || strings.Contains(w, "A old") || per[0]["read_at"] != now.Format(time.RFC3339) {
		t.Fatalf("a searched competitor gets fresh winners: %v", per[0])
	}
	if per[1]["read_at"] != "2026-11-01T06:00:00Z" || fmt.Sprint(per[1]["winners"]) == "[]" {
		t.Fatalf("an unsearched competitor keeps its earlier winners: %v", per[1])
	}
	if per[2]["error"] != "serpapi 500" || per[3]["pending"] != true {
		t.Fatalf("a failed new competitor shows the error, an unsearched new one is pending: %v %v", per[2], per[3])
	}
	if got["share_used"] != true {
		t.Fatalf("share_used is recorded: %v", got)
	}
}

func containsID(s, id string) bool {
	for i := 0; i+len(id) <= len(s); i++ {
		if s[i:i+len(id)] == id {
			return true
		}
	}
	return false
}
