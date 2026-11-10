package publishing

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

// recorder answers every request itself and remembers the appsecret_proof it carried, so a
// request to graph.facebook.com never leaves the test.
type recorder struct{ proof string }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.proof = req.URL.Query().Get("appsecret_proof")
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

// With "Require app secret" on in the Meta app, every Graph call made with one of our tokens must
// carry appsecret_proof; the publishers get it from WithProof (the Instagram publisher shares the
// Facebook transport).
func TestPublishersSignGraphCalls(t *testing.T) {
	secret := func(context.Context) string { return "app-secret" }
	want := meta.Proof("page-token", "app-secret")

	rec := &recorder{}
	fb := NewFacebookReels(meta.GraphURL)
	fb.HTTP.Transport = rec
	fb.WithProof(secret)
	if _, err := fb.HTTP.Get("https://graph.facebook.com/v26.0/me?access_token=page-token"); err != nil || rec.proof != want {
		t.Fatalf("facebook: proof %q want %q (%v)", rec.proof, want, err)
	}

	rec = &recorder{}
	ig := NewInstagramReels(meta.GraphURL)
	ig.fb.HTTP.Transport = rec
	ig.WithProof(secret)
	if _, err := ig.fb.HTTP.Get("https://graph.facebook.com/v26.0/me?access_token=page-token"); err != nil || rec.proof != want {
		t.Fatalf("instagram: proof %q want %q (%v)", rec.proof, want, err)
	}
}
