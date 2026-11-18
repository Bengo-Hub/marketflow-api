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

// A fresh winner left untagged this week (details budget used) keeps its earlier tags by creative
// id, and the summary is replaced only by a new one.
func TestMergeScanCarriesTagsAndSummary(t *testing.T) {
	now := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	meta := map[string]any{"competitor_scan": map[string]any{"summary": "old brief", "competitors": []any{
		map[string]any{"query": "a.co.ke", "winners": []any{map[string]any{"ad_creative_id": "CR1", "tags": map[string]any{"hook_type": "question"}}}},
	}}}
	var out scanResult
	if err := json.Unmarshal([]byte(`{"results":[{"query":"a.co.ke","ads":[
		{"ad_creative_id":"CR1","format":"video","total_days_shown":50},
		{"ad_creative_id":"CR2","format":"image","total_days_shown":20,"tags":{"angle":"pain"}}]}]}`), &out); err != nil {
		t.Fatal(err)
	}
	got := mergeScan(meta, []string{"a.co.ke"}, out, now)
	winners := got["competitors"].([]map[string]any)[0]["winners"].([]scanAd)
	if winners[0].Tags["hook_type"] != "question" || winners[1].Tags["angle"] != "pain" {
		t.Fatalf("earlier tags carried by creative id, fresh tags kept: %+v", winners)
	}
	if got["summary"] != "old brief" {
		t.Fatalf("no new summary keeps the old one: %v", got["summary"])
	}
	out.Summary = "new brief"
	if got := mergeScan(meta, []string{"a.co.ke"}, out, now); got["summary"] != "new brief" {
		t.Fatalf("a new summary replaces the old: %v", got["summary"])
	}
}

// The scan request carries the tenant's brand line for the relevance tag.
func TestCompetitorScanSendsBrand(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	id := seedTenant(t, db, "cscan-brand")
	db.Tenant.UpdateOneID(id).SetMetadata(map[string]any{
		"competitors":       []string{"rival.co.ke"},
		"marketing_context": map[string]any{"positioning": "Fresh cakes in Nakuru", "offers": []string{"Birthday cakes"}},
	}).ExecX(ctx)
	var brand string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Brand string `json:"brand"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		brand = body.Brand
		_, _ = w.Write([]byte(`{"results":[],"summary":"brief"}`))
	}))
	defer srv.Close()
	c := NewCompetitorScan(db, srv.URL, "k", zap.NewNop())
	if _, err := c.scan(ctx, db.Tenant.GetX(ctx, id)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brand, "Fresh cakes in Nakuru") || !strings.Contains(brand, "Birthday cakes") {
		t.Fatalf("the brand line goes with the scan: %q", brand)
	}
	scan, _ := db.Tenant.GetX(ctx, id).Metadata["competitor_scan"].(map[string]any)
	if scan["summary"] != "brief" {
		t.Fatalf("the summary is stored: %v", scan)
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
