// Package reqid carries one request id across Codevertex services (Sprint 7 #71): the id of
// the incoming request (httpware.RequestID), or of the event a worker is handling, is sent as
// X-Request-ID on calls to marketflow-ai and notifications-api, so their logs line up.
package reqid

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/nats-io/nats.go"
)

// Transport adds the context's request id to each outgoing request that has none.
type Transport struct {
	Base http.RoundTripper
}

func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if id := httpware.GetRequestID(r.Context()); id != "" && r.Header.Get(httpware.HeaderRequestID) == "" {
		r = r.Clone(r.Context()) // a RoundTripper must not change the caller's request
		r.Header.Set(httpware.HeaderRequestID, id)
	}
	return base.RoundTrip(r)
}

// Client is an HTTP client for calls to other Codevertex services. Only use it for those: the
// request id is internal and is not sent to third parties.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport{}}
}

// WithEvent makes the event's id the request id while a worker handles it: the outbox
// "event-id" header, or the envelope's "id" in the body.
func WithEvent(ctx context.Context, h nats.Header, data []byte) context.Context {
	id := ""
	if h != nil {
		id = h.Get("event-id")
	}
	if id == "" {
		var body struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &body) == nil {
			id = body.ID
		}
	}
	if id == "" || len(id) > 64 {
		return ctx
	}
	return httpware.WithRequestID(ctx, id)
}
