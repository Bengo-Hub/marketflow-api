package tenant

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestFetchFromAuthSlugGuardAndNegativeCache(t *testing.T) {
	var calls atomic.Int32
	var lastPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastPath.Store(r.URL.EscapedPath())
		if r.URL.Path == "/api/v1/tenants/by-slug/known-shop" {
			_, _ = w.Write([]byte(`{"id":"8a7c9d4e-1111-4222-8333-944455556666","slug":"known-shop","name":"Known"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	s := &Syncer{authAPIURL: srv.URL, httpClient: srv.Client(), logger: zap.NewNop()}

	if _, err := s.fetchFromAuth("known-shop"); err != nil {
		t.Fatalf("known slug: %v", err)
	}

	// A slug that is not a slug never reaches auth-api, so it cannot steer the path.
	before := calls.Load()
	for _, bad := range []string{"../admin/tenants", "a/b", "x%2F..", "", "a b"} {
		if _, err := s.fetchFromAuth(bad); !errors.Is(err, ErrUnknownTenant) {
			t.Fatalf("%q: want ErrUnknownTenant, got %v", bad, err)
		}
	}
	if calls.Load() != before {
		t.Fatalf("invalid slugs reached auth-api (%d calls)", calls.Load()-before)
	}

	// An unknown slug costs one auth-api call, then is answered from the negative cache.
	before = calls.Load()
	for i := 0; i < 5; i++ {
		if _, err := s.fetchFromAuth("made-up-shop"); !errors.Is(err, ErrUnknownTenant) {
			t.Fatalf("unknown slug: want ErrUnknownTenant, got %v", err)
		}
	}
	if got := calls.Load() - before; got != 1 {
		t.Fatalf("unknown slug asked auth-api %d times, want 1", got)
	}

	// After the TTL it is asked again.
	unknownSlugs.Lock()
	unknownSlugs.until["made-up-shop"] = time.Now().Add(-time.Second)
	unknownSlugs.Unlock()
	if _, err := s.fetchFromAuth("made-up-shop"); !errors.Is(err, ErrUnknownTenant) {
		t.Fatalf("after ttl: %v", err)
	}
	if got := calls.Load() - before; got != 2 {
		t.Fatalf("after the TTL auth-api calls = %d, want 2", got)
	}
}
