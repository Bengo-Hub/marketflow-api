package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func containsID(s, id string) bool {
	for i := 0; i+len(id) <= len(s); i++ {
		if s[i:i+len(id)] == id {
			return true
		}
	}
	return false
}
