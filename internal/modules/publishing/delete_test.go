package publishing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGraph answers DELETE /v27.0/{id} the way the Graph API documents it: {"success": true} for
// a known object, Meta's error envelope otherwise.
func fakeGraph(t *testing.T, answers map[string]struct {
	code int
	body string
}) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method %s, want DELETE", r.Method)
		}
		if r.URL.Query().Get("access_token") != "tok" {
			t.Errorf("no page token on %s", r.URL)
		}
		seen = append(seen, r.URL.Path)
		a, ok := answers[r.URL.Path]
		if !ok {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.code)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestFacebookDelete(t *testing.T) {
	srv, seen := fakeGraph(t, map[string]struct {
		code int
		body string
	}{
		"/v27.0/vid9":  {200, `{"success":true}`},
		"/v27.0/gone1": {400, `{"error":{"message":"Unsupported delete request. Object with ID 'gone1' does not exist, cannot be loaded due to missing permissions, or does not support this operation.","type":"GraphMethodException","code":100,"error_subcode":33}}`},
		"/v27.0/perm1": {403, `{"error":{"message":"(#200) Permissions error","type":"OAuthException","code":200}}`},
		"/v27.0/no1":   {200, `{"success":false}`},
	})
	fb := NewFacebookReels(srv.URL + "/v27.0")
	acct := Account{Token: "tok", ExternalID: "page1"}
	ctx := context.Background()

	if err := fb.Delete(ctx, acct, "vid9"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Graph's "does not exist" also means "missing permissions", so it is a platform error with
	// Meta's message, never a silent success.
	if err := fb.Delete(ctx, acct, "gone1"); err == nil || errors.Is(err, ErrAlreadyGone) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("an ambiguous Graph answer must come back as Meta said it: %v", err)
	}
	err := fb.Delete(ctx, acct, "perm1")
	if err == nil || !strings.Contains(err.Error(), "Permissions error") {
		t.Fatalf("Meta's message must come back: %v", err)
	}
	if err := fb.Delete(ctx, acct, "no1"); err == nil {
		t.Fatal("success false is a failure")
	}
	if err := fb.Delete(ctx, acct, ""); err == nil {
		t.Fatal("no id, no call")
	}
	if len(*seen) != 4 {
		t.Fatalf("calls: %v", *seen)
	}
}

func TestInstagramDelete(t *testing.T) {
	srv, _ := fakeGraph(t, map[string]struct {
		code int
		body string
	}{
		"/v27.0/17918920912340654": {200, `{"success":true,"deleted_id":"17918920912340654"}`},
		"/v27.0/ad1":               {400, `{"error":{"message":"Fatal","type":"OAuthException","code":-1,"error_subcode":2207073,"is_transient":false,"error_user_title":"Media Type Not Supported","error_user_msg":"The media type is not supported for this endpoint."}}`},
	})
	ig := NewInstagramReels(srv.URL + "/v27.0")
	acct := Account{Token: "tok", ExternalID: "ig1"}
	if err := ig.Delete(context.Background(), acct, "17918920912340654"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	err := ig.Delete(context.Background(), acct, "ad1")
	if err == nil || !strings.Contains(err.Error(), "media type is not supported") {
		t.Fatalf("Meta's user message must come back: %v", err)
	}
}

func TestYouTubeDelete(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete || r.URL.Path != "/youtube/v3/videos" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("request %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.URL.Query().Get("id") {
		case "v1":
			w.WriteHeader(http.StatusNoContent)
		case "gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"The video identified by the id parameter cannot be found.","errors":[{"reason":"videoNotFound"}]}}`))
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"The video that you are trying to delete cannot be deleted.","errors":[{"reason":"forbidden"}]}}`))
		}
	}))
	defer srv.Close()
	y := NewYouTubeShorts(srv.URL+"/upload/youtube/v3", srv.URL+"/youtube/v3", "")
	acct := Account{Token: "tok"}
	ctx := context.Background()
	if err := y.Delete(ctx, acct, "v1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := y.Delete(ctx, acct, "gone"); !errors.Is(err, ErrAlreadyGone) {
		t.Fatalf("404 is already gone: %v", err)
	}
	err := y.Delete(ctx, acct, "locked")
	if err == nil || !strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), "cannot be deleted") {
		t.Fatalf("Google's reason and message must come back: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls %d", calls)
	}
}

func TestTikTokDeleteUnsupported(t *testing.T) {
	err := NewTikTokDirect("http://127.0.0.1:1", "", false).Delete(context.Background(), Account{Token: "tok"}, "7300000000000000000")
	if !errors.Is(err, ErrDeleteUnsupported) {
		t.Fatalf("TikTok has no delete: %v", err)
	}
	var un *DeleteUnsupportedError
	if !errors.As(err, &un) || un.Platform != TikTok || len(un.Steps) == 0 || un.HelpURL == "" {
		t.Fatalf("the manual steps travel with the refusal: %+v", un)
	}
}

func TestDeleteSupport(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		scopes   []string
		want     bool
	}{
		{"facebook with pages_manage_posts", Facebook, []string{"pages_show_list", "pages_read_engagement", "pages_manage_posts"}, true},
		{"facebook without it", Facebook, []string{"pages_show_list"}, false},
		{"instagram publish only", Instagram, []string{"instagram_basic", "instagram_content_publish"}, false},
		{"instagram manage contents", Instagram, []string{"instagram_basic", "instagram_content_publish", "instagram_manage_contents"}, true},
		{"youtube upload only", YouTube, []string{"https://www.googleapis.com/auth/youtube.upload"}, false},
		{"youtube force-ssl", YouTube, []string{"https://www.googleapis.com/auth/youtube.upload", "https://www.googleapis.com/auth/youtube.force-ssl"}, true},
		{"youtube full", YouTube, []string{"https://www.googleapis.com/auth/youtube"}, true},
		{"tiktok never", TikTok, []string{"video.publish", "video.list"}, false},
		{"unknown", "myspace", nil, false},
	}
	for _, c := range cases {
		info := DeleteSupport(c.platform, c.scopes)
		if info.CanDelete != c.want {
			t.Errorf("%s: can %v, want %v (%s)", c.name, info.CanDelete, c.want, info.Reason)
		}
		if !info.CanDelete && info.Reason == "" {
			t.Errorf("%s: a refusal always says why", c.name)
		}
		if c.platform != "myspace" && len(info.Steps) == 0 {
			t.Errorf("%s: every platform has manual steps", c.name)
		}
	}

	// The YouTube connection's scopes both post (preflight) and delete.
	if missing := MissingScopes(YouTube, YouTubeConnectScopes); len(missing) != 0 {
		t.Errorf("YouTubeConnectScopes cannot post: %v", missing)
	}
	if !DeleteSupport(YouTube, YouTubeConnectScopes).CanDelete {
		t.Error("YouTubeConnectScopes must include a delete scope")
	}

	// Reconnect hints: only where the platform can delete, the stored scopes are known and lack it.
	igPublish := []string{"instagram_basic", "instagram_content_publish", "pages_read_engagement"}
	if !NeedsReconnectToDelete(Instagram, igPublish) {
		t.Error("an Instagram account connected before instagram_manage_contents must be told to reconnect")
	}
	if NeedsReconnectToDelete(Instagram, append(igPublish, "instagram_manage_contents")) {
		t.Error("granted, no hint")
	}
	if !NeedsReconnectToDelete(YouTube, []string{"https://www.googleapis.com/auth/youtube.upload"}) {
		t.Error("a YouTube channel with upload only must be told to reconnect")
	}
	if NeedsReconnectToDelete(TikTok, []string{"video.publish"}) || NeedsReconnectToDelete(Instagram, nil) {
		t.Error("no hint where reconnecting cannot help or the scopes are unknown")
	}

	// The job's own conditions come on top.
	fbScopes := []string{"pages_manage_posts"}
	if deleteInfoFor(Facebook, "", true, fbScopes).CanDelete {
		t.Error("no stored id, no delete")
	}
	if deleteInfoFor(Facebook, "vid9", false, fbScopes).CanDelete {
		t.Error("a disconnected account cannot delete")
	}
	if !deleteInfoFor(Facebook, "vid9", true, fbScopes).CanDelete {
		t.Error("a connected page with the permission can delete")
	}
}
