package funnels

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func stepView(h *AnalyticsHandler, slug, body string) int {
	r := chi.NewRouter()
	r.Post("/public/funnels/{slug}/step-view", h.RecordStepView)
	req := httptest.NewRequest(http.MethodPost, "/public/funnels/"+slug+"/step-view", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

// A slug that cannot be a funnel link is refused before Redis is touched (the handler has no
// Redis here, so reaching it would panic).
func TestStepViewRefusesBadSlugs(t *testing.T) {
	h := NewAnalyticsHandler(nil, nil, nil, zap.NewNop())
	for _, slug := range []string{strings.Repeat("a", 61), "Bad_Slug", "has.dot"} {
		if code := stepView(h, slug, `{"step_index":0}`); code != http.StatusBadRequest {
			t.Fatalf("slug %q: got %d, want 400", slug, code)
		}
	}
}

// An unknown slug writes no counter, and the answer is cached; a counted view carries a TTL.
// Needs local Redis (DB 15); skipped without it.
func TestStepViewCountsOnlyPublishedFunnels(t *testing.T) {
	if c, err := net.DialTimeout("tcp", "127.0.0.1:6379", time.Second); err != nil {
		t.Skip("local redis unavailable")
	} else {
		_ = c.Close()
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	slug := "sweep-unknown-funnel"
	t.Cleanup(func() {
		_ = rdb.Del(ctx, stepViewRedisKey(slug, 0), publishedKeyBase+slug).Err()
	})

	// No database: no slug can be confirmed, so nothing is counted.
	h := NewAnalyticsHandler(nil, rdb, nil, zap.NewNop())
	if code := stepView(h, slug, `{"step_index":0}`); code != http.StatusNoContent {
		t.Fatalf("an unknown slug still answers 204, got %d", code)
	}
	if n, _ := rdb.Exists(ctx, stepViewRedisKey(slug, 0)).Result(); n != 0 {
		t.Fatal("an unknown slug must not create a counter")
	}

	// A slug cached as published is counted, with a TTL.
	if err := rdb.Set(ctx, publishedKeyBase+slug, publishedYes, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if code := stepView(h, slug, `{"step_index":0}`); code != http.StatusNoContent {
		t.Fatalf("got %d", code)
	}
	if v, _ := rdb.Get(ctx, stepViewRedisKey(slug, 0)).Int(); v != 1 {
		t.Fatalf("one view counted, got %d", v)
	}
	if ttl, _ := rdb.TTL(ctx, stepViewRedisKey(slug, 0)).Result(); ttl <= 0 || ttl > stepViewTTL {
		t.Fatalf("the counter has a TTL, got %v", ttl)
	}
}
