package google

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuthCodeURL(t *testing.T) {
	u, err := url.Parse(AuthCodeURL(Client{ID: "cid", RedirectURL: "https://api.example.com/cb"},
		[]string{"https://www.googleapis.com/auth/youtube.upload", "https://www.googleapis.com/auth/youtube.force-ssl"}, "st"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("state") != "st" ||
		q.Get("redirect_uri") != "https://api.example.com/cb" || q.Get("response_type") != "code" {
		t.Errorf("a refresh token needs offline access and consent every time: %v", q)
	}
	if q.Get("scope") != "https://www.googleapis.com/auth/youtube.upload https://www.googleapis.com/auth/youtube.force-ssl" {
		t.Errorf("scopes are space separated: %q", q.Get("scope"))
	}
}

func TestExchangeAndRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code") == "good":
			_, _ = w.Write([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":3599,"scope":"a b"}`))
		case r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "RT":
			_, _ = w.Write([]byte(`{"access_token":"AT2","expires_in":3599}`))
		case r.Form.Get("refresh_token") == "DOWN":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
		}
	}))
	defer srv.Close()
	o := OAuth{TokenURL: srv.URL}
	c := Client{ID: "cid", Secret: "sec", RedirectURL: "https://x/cb"}
	ctx := context.Background()

	tok, err := o.Exchange(ctx, c, "good")
	if err != nil || tok.Access != "AT" || tok.Refresh != "RT" || tok.ExpiresIn != 3599*time.Second || len(tok.Scopes) != 2 {
		t.Fatalf("exchange: %+v %v", tok, err)
	}
	tok, err = o.Refresh(ctx, c, "RT")
	if err != nil || tok.Access != "AT2" || tok.Refresh != "" {
		t.Fatalf("refresh keeps the same refresh token: %+v %v", tok, err)
	}
	_, err = o.Refresh(ctx, c, "GONE")
	var te *TokenError
	if !errors.As(err, &te) || !te.Revoked() || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("a revoked grant is a TokenError naming invalid_grant: %v", err)
	}
	_, err = o.Refresh(ctx, c, "DOWN")
	if err == nil || errors.As(err, &te) {
		t.Fatalf("a 5xx is a plain error, never a reason to disconnect: %v", err)
	}
}

func TestMyChannels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer AT" || r.URL.Query().Get("mine") != "true" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid Credentials"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"UC1","snippet":{"title":"Shop TV","customUrl":"@shoptv",
			"thumbnails":{"default":{"url":"https://yt3/d.jpg"},"medium":{"url":"https://yt3/m.jpg"}}}},{"id":""}]}`))
	}))
	defer srv.Close()
	ch, err := MyChannels(context.Background(), nil, srv.URL, "AT")
	if err != nil || len(ch) != 1 {
		t.Fatalf("%+v %v", ch, err)
	}
	if ch[0].ID != "UC1" || ch[0].Title != "Shop TV" || ch[0].CustomURL != "@shoptv" || ch[0].Thumbnail != "https://yt3/m.jpg" {
		t.Errorf("channel: %+v", ch[0])
	}
	if _, err := MyChannels(context.Background(), nil, srv.URL, "BAD"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("an error answer is reported: %v", err)
	}
}

func TestRevokeTreatsAlreadyRevokedAsDone(t *testing.T) {
	codes := map[string]int{"live": http.StatusOK, "old": http.StatusBadRequest, "down": http.StatusInternalServerError}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.WriteHeader(codes[r.Form.Get("token")])
	}))
	defer srv.Close()
	o := OAuth{RevokeURL: srv.URL}
	if err := o.Revoke(context.Background(), "live"); err != nil {
		t.Error(err)
	}
	if err := o.Revoke(context.Background(), "old"); err != nil {
		t.Error("an already revoked token is the outcome we want:", err)
	}
	if err := o.Revoke(context.Background(), "down"); err == nil {
		t.Error("a 5xx is reported")
	}
}

func TestHasScopes(t *testing.T) {
	if !HasScopes([]string{"a", "b", "c"}, []string{"a", "c"}) || HasScopes([]string{"a"}, []string{"a", "b"}) {
		t.Error("HasScopes")
	}
}
