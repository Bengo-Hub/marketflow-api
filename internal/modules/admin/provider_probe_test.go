package admin

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestProbeOfflineBranches(t *testing.T) {
	c := &http.Client{Timeout: time.Second}
	ctx := context.Background()
	if _, ok := probeMedia(ctx, c, "fal", ""); ok {
		t.Fatal("a missing key passed")
	}
	if _, ok := probeMedia(ctx, c, "nope", "k"); ok {
		t.Fatal("an unknown media provider passed")
	}
	// Without a test event code nothing is sent to Meta.
	if msg, ok := probeMetaCAPI(ctx, c, map[string]string{"dataset_id": "1", "access_token": "t"}); ok || msg == "" {
		t.Fatalf("no test code: %q %v", msg, ok)
	}
	if _, ok := runProbe(ctx, "meta_ads", "threads", map[string]string{"app_id": "1"}); ok {
		t.Fatal("Threads without a secret passed")
	}
}

// Live check of the media probes, run only when the keys are in the environment
// (MF_FAL_API_KEY, MF_HIGGSFIELD_API_KEY, MF_ELEVEN_LABS_API_KEY, MF_CREATOMATE_API_KEY).
func TestProbeMediaLive(t *testing.T) {
	keys := map[string]string{
		"fal": os.Getenv("MF_FAL_API_KEY"), "higgsfield": os.Getenv("MF_HIGGSFIELD_API_KEY"),
		"elevenlabs": os.Getenv("MF_ELEVEN_LABS_API_KEY"), "creatomate": os.Getenv("MF_CREATOMATE_API_KEY"),
	}
	c := &http.Client{Timeout: 20 * time.Second}
	ran := false
	for p, k := range keys {
		if k == "" {
			continue
		}
		ran = true
		if msg, ok := probeMedia(context.Background(), c, p, k); !ok {
			t.Errorf("%s: %s", p, msg)
		} else {
			t.Logf("%s: %s", p, msg)
		}
		if _, ok := probeMedia(context.Background(), c, p, "bad:bad"); ok {
			t.Errorf("%s accepted a bad key", p)
		}
	}
	if !ran {
		t.Skip("no media keys in the environment")
	}
}
