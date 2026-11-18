package webhooks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// The subscription check answers the challenge only for the configured token, and never when no
// token is configured.
func TestMetaVerifyToken(t *testing.T) {
	verify := func(configured, sent string) int {
		h := NewMetaHandler(nil, MetaConfig{VerifyToken: configured}, zap.NewNop())
		req := httptest.NewRequest(http.MethodGet, "/webhooks/meta/leadgen?hub.mode=subscribe&hub.verify_token="+sent+"&hub.challenge=42", nil)
		rec := httptest.NewRecorder()
		h.Verify(rec, req)
		return rec.Code
	}
	if code := verify("tok", "tok"); code != http.StatusOK {
		t.Fatalf("the right token answers 200, got %d", code)
	}
	if code := verify("tok", "tik"); code == http.StatusOK {
		t.Fatal("a wrong token must be refused")
	}
	if code := verify("", ""); code == http.StatusOK {
		t.Fatal("no configured token means no subscription")
	}
}
