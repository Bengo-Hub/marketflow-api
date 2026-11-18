package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// The alert reads the Slack channel from auth-api with the service key and posts the text there.
// The webhook check accepts only https, so the fake Slack is a TLS server.
func TestPlatformAlertPostsToTheConfiguredSlack(t *testing.T) {
	var posted string
	slack := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = body["text"]
	}))
	defer slack.Close()
	var key string
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("X-API-Key")
		_ = json.NewEncoder(w).Encode(map[string]string{"slack_webhook_url": slack.URL})
	}))
	defer auth.Close()

	a := NewPlatformAlert(nil, nil, auth.URL, "s2s", "codevertex", zap.NewNop())
	a.hc = slack.Client() // trusts the fake's certificate; auth-api is plain http here
	a.Send(context.Background(), "Storage", "live bytes are high", "media_live_bytes", "2026-11-18")
	if key != "s2s" || posted != "live bytes are high" {
		t.Fatalf("service key sent %q, slack got %q", key, posted)
	}
}

func TestPlatformAlertSkipsWithoutAuthSettings(t *testing.T) {
	a := NewPlatformAlert(nil, nil, "", "", "codevertex", zap.NewNop())
	a.Send(context.Background(), "s", "t", "k", "d") // nothing configured: no call, no panic
}
