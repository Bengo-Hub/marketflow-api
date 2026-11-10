package integrations

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type graphStub map[string]string // path -> JSON body

func (g graphStub) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := g[r.URL.Path]
	if !ok {
		body = `{"error":{"message":"unexpected path ` + r.URL.Path + `"}}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// Production 2026-11-10: Facebook Login for Business connected a business portfolio, the code
// exchange worked, but /me/accounts came back empty and the callback answered no_pages. The
// Pages a person granted are listed per permission in debug_token granular_scopes; each is then
// read directly with the user token to get its Page token.
func TestGrantedPagesFallsBackToGranularScopes(t *testing.T) {
	prev := graphClient
	t.Cleanup(func() { graphClient = prev })
	base := strings.TrimPrefix(metaGraphURL, "https://graph.facebook.com")
	graphClient = &http.Client{Transport: graphStub{
		base + "/debug_token": `{"data":{"granular_scopes":[
			{"scope":"pages_show_list","target_ids":["111","222"]},
			{"scope":"pages_manage_posts","target_ids":["222","333"]},
			{"scope":"ads_management","target_ids":["act_9"]},
			{"scope":"business_management"}]}}`,
		base + "/111": `{"id":"111","name":"Codevertex Africa","access_token":"FAKE-PT-111"}`,
		base + "/222": `{"id":"222","name":"Shop","access_token":"FAKE-PT-222","instagram_business_account":{"id":"ig2","username":"shop"}}`,
		base + "/333": `{"error":{"message":"no access"}}`,
	}}

	pages, err := grantedPages(context.Background(), "FAKE-USER", "FAKE-APP")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[0].ID != "111" || pages[1].ID != "222" {
		t.Fatalf("want Pages 111 and 222 (333 unreadable, act_9 not a Page scope): %+v", pages)
	}
	if pages[0].AccessToken != "FAKE-PT-111" || pages[1].Instagram == nil || pages[1].Instagram.ID != "ig2" {
		t.Fatalf("page token and Instagram account kept: %+v", pages)
	}
}

func TestGrantedPagesNoPageScopes(t *testing.T) {
	prev := graphClient
	t.Cleanup(func() { graphClient = prev })
	base := strings.TrimPrefix(metaGraphURL, "https://graph.facebook.com")
	graphClient = &http.Client{Transport: graphStub{
		base + "/debug_token": `{"data":{"granular_scopes":[{"scope":"business_management"}]}}`,
	}}
	pages, err := grantedPages(context.Background(), "FAKE-USER", "FAKE-APP")
	if err != nil || len(pages) != 0 {
		t.Fatalf("no Page grants means no Pages: %v %+v", err, pages)
	}
}

// A reload after the 10 minute window must say the link expired, not that the signature is bad.
func TestStateExpiredIsDistinct(t *testing.T) {
	h := &Handler{stateKey: []byte("k")}
	old := h.signStateAt("tenant", "user", 1)
	if _, _, err := h.checkState(old); err != errStateExpired {
		t.Fatalf("old state: want errStateExpired, got %v", err)
	}
	if _, _, err := h.checkState("tenant.user.1.deadbeef"); err != errStateInvalid {
		t.Fatalf("bad sig: want errStateInvalid, got %v", err)
	}
}

// Production 2026-11-10: the Meta callback redirected to "/codevertex/settings/..." on the API
// host, which answered 404. The return page lives on the portal.
func TestIntegrationsURLIsOnThePortal(t *testing.T) {
	h := (&Handler{}).WithPortal("https://marketflow.example.com/")
	got := h.integrationsURL(context.Background(), "codevertex", "meta=connected")
	if got != "https://marketflow.example.com/codevertex/settings/integrations?meta=connected" {
		t.Fatalf("got %q", got)
	}
}
