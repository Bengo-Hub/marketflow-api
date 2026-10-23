package worker

import (
	"testing"
	"time"
)

func TestRetryDelaySpreadsRetries(t *testing.T) {
	want := map[uint64]time.Duration{0: 10 * time.Second, 1: 10 * time.Second, 2: time.Minute, 3: 5 * time.Minute, 4: 10 * time.Minute, 9: 10 * time.Minute}
	for n, d := range want {
		if got := retryDelay(n); got != d {
			t.Errorf("delivery %d: %v, want %v", n, got, d)
		}
	}
	var total time.Duration
	for n := uint64(1); n < consumerMaxDeliver; n++ {
		total += retryDelay(n)
	}
	// The retries must outlast a short downstream outage, not burn out in seconds.
	if total < 10*time.Minute {
		t.Errorf("retries span only %v", total)
	}
	if consumerAckWait < 90*time.Second {
		t.Errorf("ack wait %v is below the slowest handler's bound", consumerAckWait)
	}
}
