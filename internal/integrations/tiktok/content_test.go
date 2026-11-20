package tiktok

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// Fake fixture values only.
var testApp = App{Key: "test-key", Secret: "test-secret", RedirectURL: "https://api.example.test/api/v1/integrations/tiktok-creator/callback"} // secret-scan:allow

func TestAuthCodeURL(t *testing.T) {
	u, err := url.Parse(AuthCodeURL(testApp, ConnectScopes, "st"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != AuthorizeURL {
		t.Fatalf("endpoint %s", u)
	}
	if q.Get("client_key") != "test-key" || q.Get("response_type") != "code" || q.Get("state") != "st" ||
		q.Get("redirect_uri") != testApp.RedirectURL || q.Get("scope") != "user.info.basic,video.publish,video.upload" {
		t.Fatalf("query %v", q)
	}
	if q.Has("client_id") {
		t.Fatal("TikTok takes client_key")
	}
}

func TestExchangeRefreshUserRevoke(t *testing.T) {
	var lastForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		lastForm = r.PostForm
		switch r.URL.Path {
		case "/token":
			if r.PostForm.Get("refresh_token") == "dead" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Refresh token is invalid or expired.","log_id":"L9"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"act.1","expires_in":86400,"refresh_token":"rft.2","refresh_expires_in":31536000,"open_id":"open-1","scope":"user.info.basic,video.publish,video.upload","token_type":"Bearer"}`))
		case "/revoke":
			w.WriteHeader(http.StatusOK)
		case "/v2/user/info/":
			if r.Header.Get("Authorization") != "Bearer act.1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"access_token_invalid","message":"bad","log_id":"L1"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"user":{"open_id":"open-1","union_id":"u-1","display_name":"Duka","avatar_url":"https://p.example/a.jpg"}},"error":{"code":"ok"}}`))
		}
	}))
	defer srv.Close()
	o := OAuth{TokenURL: srv.URL + "/token", RevokeURL: srv.URL + "/revoke", APIBase: srv.URL}
	ctx := context.Background()

	tok, err := o.Exchange(ctx, testApp, "code-1")
	if err != nil {
		t.Fatal(err)
	}
	if lastForm.Get("client_key") != "test-key" || lastForm.Get("grant_type") != "authorization_code" || lastForm.Get("code") != "code-1" ||
		lastForm.Get("redirect_uri") != testApp.RedirectURL {
		t.Fatalf("exchange form %v", lastForm)
	}
	if tok.Access != "act.1" || tok.Refresh != "rft.2" || tok.OpenID != "open-1" || tok.ExpiresIn != 24*time.Hour ||
		!HasScopes(tok.Scopes, ConnectScopes) {
		t.Fatalf("token %+v", tok)
	}

	if _, err := o.Refresh(ctx, testApp, "rft.2"); err != nil || lastForm.Get("grant_type") != "refresh_token" {
		t.Fatalf("refresh err %v form %v", err, lastForm)
	}
	_, err = o.Refresh(ctx, testApp, "dead")
	var te *TokenError
	if !errors.As(err, &te) || !te.Revoked() {
		t.Fatalf("dead refresh token must be a revoked TokenError, got %v", err)
	}

	u, err := o.UserInfo(ctx, "act.1")
	if err != nil || u.OpenID != "open-1" || u.DisplayName != "Duka" || u.UnionID != "u-1" {
		t.Fatalf("user %+v err %v", u, err)
	}
	if _, err := o.UserInfo(ctx, "other"); err == nil {
		t.Fatal("invalid token must fail")
	}

	if err := o.Revoke(ctx, testApp, "act.1"); err != nil || lastForm.Get("token") != "act.1" || lastForm.Get("client_secret") != "test-secret" {
		t.Fatalf("revoke err %v form %v", err, lastForm)
	}
}

func TestServerErrorIsNotRevoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := OAuth{TokenURL: srv.URL}.Refresh(context.Background(), testApp, "r")
	var te *TokenError
	if err == nil || errors.As(err, &te) {
		t.Fatalf("a 502 must be a retryable error, got %v", err)
	}
}

func TestScopes(t *testing.T) {
	if got := SplitScopes("user.info.basic, video.publish,,video.upload"); len(got) != 3 {
		t.Fatalf("split %v", got)
	}
	if HasScopes([]string{"user.info.basic", "video.upload"}, ConnectScopes) {
		t.Fatal("missing video.publish must fail")
	}
}

func TestVerifySignature(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"event":"post.publish.complete"}`)
	sign := func(ts int64, secret string) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(strconv.FormatInt(ts, 10) + "."))
		m.Write(body)
		return "t=" + strconv.FormatInt(ts, 10) + ",s=" + hex.EncodeToString(m.Sum(nil))
	}
	if !VerifySignature(sign(now.Unix(), "test-secret"), body, "test-secret", now) {
		t.Fatal("valid signature refused")
	}
	if VerifySignature(sign(now.Unix(), "other"), body, "test-secret", now) {
		t.Fatal("wrong key accepted")
	}
	if VerifySignature(sign(now.Add(-10*time.Minute).Unix(), "test-secret"), body, "test-secret", now) {
		t.Fatal("old event accepted")
	}
	if VerifySignature(sign(now.Unix(), "test-secret"), []byte(`{"event":"x"}`), "test-secret", now) {
		t.Fatal("changed body accepted")
	}
	if VerifySignature("", body, "test-secret", now) || VerifySignature(sign(now.Unix(), ""), body, "", now) {
		t.Fatal("empty header or secret accepted")
	}
}
