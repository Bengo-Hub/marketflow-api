package inbox

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

type recorder struct{ proof string }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.proof = req.URL.Query().Get("appsecret_proof")
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

// Inbox replies use the tenant's Page token, so they must carry appsecret_proof once the Meta app
// requires it.
func TestInboxGraphSignsCalls(t *testing.T) {
	rec := &recorder{}
	g := NewGraph(meta.GraphURL)
	g.HTTP.Transport = rec
	g.WithProof(func(context.Context) string { return "app-secret" })
	if _, err := g.HTTP.Get("https://graph.facebook.com/v26.0/me?access_token=page-token"); err != nil || rec.proof != meta.Proof("page-token", "app-secret") {
		t.Fatalf("proof %q (%v)", rec.proof, err)
	}
}
