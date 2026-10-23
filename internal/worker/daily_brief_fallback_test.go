package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// When marketflow-ai fails, the brief still goes out with the numbers, and the next tenants do
// not each wait for the same failure.
func TestDailyBriefWordsBackOffAfterFailure(t *testing.T) {
	var calls atomic.Int32
	failing := atomic.Bool{}
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"insights":["Leads up"],"actions":["Reply to 3 leads"]}`))
	}))
	defer srv.Close()

	now := time.Date(2026, 10, 23, 7, 0, 0, 0, time.UTC)
	j := &DailyBrief{aiURL: srv.URL, serviceKey: "k", hc: srv.Client(), logger: zap.NewNop(), nowFn: func() time.Time { return now }}
	ctx := context.Background()

	if w := j.words(ctx, "en", nil); len(w.Insights) != 0 {
		t.Fatal("a failed call gives the numbers-only brief")
	}
	if w := j.words(ctx, "en", nil); len(w.Insights) != 0 || calls.Load() != 1 {
		t.Fatalf("after a failure the next brief skips the call: calls=%d", calls.Load())
	}
	failing.Store(false)
	now = now.Add(briefWordsBackoff + time.Second)
	if w := j.words(ctx, "en", nil); len(w.Insights) != 1 || calls.Load() != 2 {
		t.Fatalf("after the backoff the call is tried again: %+v calls=%d", w, calls.Load())
	}
}
