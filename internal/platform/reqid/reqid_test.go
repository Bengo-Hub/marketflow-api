package reqid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/httpware"
	"github.com/nats-io/nats.go"
)

// Sprint 7 #71: one id follows a request or an event across Codevertex services.
func TestClientForwardsRequestID(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(httpware.HeaderRequestID))
	}))
	defer srv.Close()
	c := Client(0)

	ctx := httpware.WithRequestID(context.Background(), "req-123")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := c.Do(req); err != nil {
		t.Fatal(err)
	}
	// A header the caller set wins.
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	req.Header.Set(httpware.HeaderRequestID, "mine")
	if _, err := c.Do(req); err != nil {
		t.Fatal(err)
	}
	// No id in the context: nothing is added.
	req, _ = http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := c.Do(req); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "req-123" || got[1] != "mine" || got[2] != "" {
		t.Fatalf("forwarded ids: %q", got)
	}
}

func TestWithEvent(t *testing.T) {
	h := nats.Header{}
	h.Set("event-id", "6f1e0b3a-6b0e-4c55-9a2f-3d5b7c1e8a90")
	if id := httpware.GetRequestID(WithEvent(context.Background(), h, nil)); id != "6f1e0b3a-6b0e-4c55-9a2f-3d5b7c1e8a90" {
		t.Fatalf("event-id header: %q", id)
	}
	if id := httpware.GetRequestID(WithEvent(context.Background(), nil, []byte(`{"id":"0b6d3c1e-1111-4c55-9a2f-3d5b7c1e8a90"}`))); id != "0b6d3c1e-1111-4c55-9a2f-3d5b7c1e8a90" {
		t.Fatalf("body id: %q", id)
	}
	if id := httpware.GetRequestID(WithEvent(context.Background(), nil, []byte(`not json`))); id != "" {
		t.Fatalf("no id: %q", id)
	}
}
