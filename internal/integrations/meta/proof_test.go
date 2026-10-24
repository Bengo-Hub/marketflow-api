package meta

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// recorder captures what reaches the network instead of sending it.
type recorder struct {
	got  *http.Request
	body string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		r.body = string(b)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

func want(token, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(token))
	return hex.EncodeToString(m.Sum(nil))
}

func TestProofTransport(t *testing.T) {
	rec := &recorder{}
	secret := func(context.Context) string { return "app-secret" }
	c := &http.Client{Transport: NewProofTransport(rec, secret)}

	// Token in the query.
	_, _ = c.Get("https://graph.facebook.com/v21.0/me?access_token=tok1&fields=id")
	if rec.got.URL.Query().Get("appsecret_proof") != want("tok1", "app-secret") {
		t.Fatalf("query: %s", rec.got.URL)
	}
	// Token in a form body (CreateCampaign).
	_, _ = c.PostForm("https://graph.facebook.com/v21.0/act_1/campaigns", url.Values{"access_token": {"tok2"}, "name": {"x"}})
	if form, _ := url.ParseQuery(rec.body); form.Get("appsecret_proof") != want("tok2", "app-secret") || form.Get("name") != "x" {
		t.Fatalf("form: %s", rec.body)
	}
	// Bearer token.
	req, _ := http.NewRequest(http.MethodPost, "https://graph.facebook.com/v21.0/1/events", nil)
	req.Header.Set("Authorization", "Bearer tok3")
	_, _ = c.Do(req)
	if rec.got.URL.Query().Get("appsecret_proof") != want("tok3", "app-secret") {
		t.Fatalf("bearer: %s", rec.got.URL)
	}
	// Other hosts and an existing proof are left alone.
	_, _ = c.Get("https://example.com/?access_token=tok4")
	if rec.got.URL.Query().Get("appsecret_proof") != "" {
		t.Fatal("proof added to a non-Graph host")
	}
	_, _ = c.Get("https://graph.facebook.com/me?access_token=tok5&appsecret_proof=keep")
	if rec.got.URL.Query().Get("appsecret_proof") != "keep" {
		t.Fatal("an existing proof was replaced")
	}
	// No secret configured: unchanged, as before the setting existed.
	none := &http.Client{Transport: NewProofTransport(rec, func(context.Context) string { return "" })}
	_, _ = none.Get("https://graph.facebook.com/me?access_token=tok6")
	if rec.got.URL.Query().Get("appsecret_proof") != "" {
		t.Fatal("proof added without a secret")
	}
}
