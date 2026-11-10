package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestNextGraphVersion(t *testing.T) {
	for in, want := range map[string]string{"v27.0": "v28.0", "v9.0": "v10.0", "bad": "", "": ""} {
		if got := nextGraphVersion(in); got != want {
			t.Errorf("nextGraphVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// The watch asks Meta for the app at the next version: a 200 means Meta has released it, and the
// platform owner is told once; anything else means MarketFlow is on the newest version.
func TestGraphVersionWatchFindsANewerVersion(t *testing.T) {
	released := map[string]bool{"/v28.0/app": true}
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.URL.Query().Get("access_token")
		if released[r.URL.Path] {
			_, _ = w.Write([]byte(`{"id":"1"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unknown path components","code":2500}}`))
	}))
	defer srv.Close()

	var told []string
	w := &GraphVersionWatch{graphRoot: srv.URL, current: "v27.0", hc: srv.Client(), logger: zap.NewNop(),
		appToken: func(context.Context) (string, bool) { return "1|secret", true },
		tell:     func(_ context.Context, current, newer string) { told = append(told, current+">"+newer) }}

	newer, err := w.check(context.Background())
	if err != nil || newer != "v28.0" || gotToken != "1|secret" {
		t.Fatalf("newer %q err %v token %q", newer, err, gotToken)
	}
	w.tick(context.Background())
	if strings.Join(told, ",") != "v27.0>v28.0" {
		t.Fatalf("told %v", told)
	}

	w.current = "v28.0"
	told = nil
	if newer, err := w.check(context.Background()); err != nil || newer != "" {
		t.Fatalf("on the newest version: %q %v", newer, err)
	}
	w.tick(context.Background())
	if len(told) != 0 {
		t.Fatalf("nothing to tell, told %v", told)
	}

	w.appToken = func(context.Context) (string, bool) { return "", false }
	if _, err := w.check(context.Background()); err == nil {
		t.Fatal("no Meta app configured must be an error, not a silent pass")
	}
}
