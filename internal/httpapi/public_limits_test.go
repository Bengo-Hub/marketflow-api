package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// perIP is a pass-through with no limiter or a 0 limit, so a values file can turn one off.
func TestPerIPPassThrough(t *testing.T) {
	called := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++ })
	for _, h := range []http.Handler{perIP(nil, "public-leads", 10)(next), perIP(nil, "public-leads", 0)(next)} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/public/leads/x", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if called != 2 {
		t.Fatalf("next called %d times", called)
	}
}
