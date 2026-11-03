package publishing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A fake Graph API that checks the documented Reels publishing shapes (start, rupload with
// file_url, finish with is_ai_generated, status).
func TestFacebookReelsFlow(t *testing.T) {
	var finish url.Values
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v26.0/page1/video_reels":
			b, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(b))
			if form.Get("access_token") != "tok" {
				t.Errorf("no page token")
			}
			switch form.Get("upload_phase") {
			case "start":
				_, _ = w.Write([]byte(`{"video_id":"vid9","upload_url":"` + srv.URL + `/video-upload/vid9"}`))
			case "finish":
				finish = form
				_, _ = w.Write([]byte(`{"success":true}`))
			default:
				t.Errorf("phase %q", form.Get("upload_phase"))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/video-upload/vid9":
			if r.Header.Get("Authorization") != "OAuth tok" || r.Header.Get("file_url") != "https://media.example/v.mp4" ||
				r.Header.Get("offset") != "0" || r.Header.Get("file_size") != "1000" {
				t.Errorf("rupload headers: %v", r.Header)
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v26.0/vid9":
			_, _ = w.Write([]byte(`{"status":{"video_status":"ready","publishing_phase":{"status":"complete","publish_status":"published"}},"permalink_url":"/reel/123"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	fb := NewFacebookReels(srv.URL + "/v26.0")
	ctx := context.Background()
	acct := Account{ExternalID: "page1", Token: "tok"}
	v := Video{URL: "https://media.example/v.mp4", Bytes: 1000}
	p := Post{Caption: "Holiday coding", AIGenerated: true}

	st, err := fb.Start(ctx, acct, v, p)
	if err != nil || st.ExternalID != "vid9" {
		t.Fatalf("start: %+v %v", st, err)
	}
	if st, err = fb.Upload(ctx, acct, v, st); err != nil || st.Offset != 1000 {
		t.Fatalf("upload: %+v %v", st, err)
	}
	if _, err = fb.Finish(ctx, acct, p, st); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if finish.Get("video_state") != "PUBLISHED" || finish.Get("is_ai_generated") != "true" || finish.Get("description") != "Holiday coding" {
		t.Fatalf("finish form: %v", finish)
	}
	res, err := fb.Status(ctx, acct, st)
	if err != nil || !res.Done || !res.Published || res.Permalink != "https://www.facebook.com/reel/123" {
		t.Fatalf("status: %+v %v", res, err)
	}
}

func TestFacebookStatusFailureAndGraphError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bad") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid OAuth access token","type":"OAuthException"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":{"video_status":"error","processing_phase":{"status":"error","errors":[{"message":"Video too short"}]}}}`))
	}))
	defer srv.Close()
	fb := NewFacebookReels(srv.URL)
	res, err := fb.Status(context.Background(), Account{Token: "t"}, UploadState{ExternalID: "vid"})
	if err != nil || !res.Done || res.Published || res.Error != "Video too short" {
		t.Fatalf("failed status: %+v %v", res, err)
	}
	if _, err := fb.Status(context.Background(), Account{Token: "t"}, UploadState{ExternalID: "bad"}); err == nil || !strings.Contains(err.Error(), "Invalid OAuth access token") {
		t.Fatalf("graph error not surfaced: %v", err)
	}
}
