package publishing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstagramReelsFlow(t *testing.T) {
	igPollEvery, igPollFor = 10*time.Millisecond, time.Second
	defer func() { igPollEvery, igPollFor = time.Minute, 5*time.Minute }()

	var polls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v26.0/ig1/content_publishing_limit":
			_, _ = w.Write([]byte(`{"data":[{"quota_usage":3,"config":{"quota_total":50}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v26.0/ig1/media":
			b, _ := io.ReadAll(r.Body)
			f, _ := url.ParseQuery(string(b))
			if f.Get("media_type") != "REELS" || f.Get("upload_type") != "resumable" || f.Get("is_ai_generated") != "true" {
				t.Errorf("container form: %v", f)
			}
			_, _ = w.Write([]byte(`{"id":"c1","uri":"` + srv.URL + `/ig-api-upload/v26.0/c1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/ig-api-upload/v26.0/c1":
			if r.Header.Get("file_url") == "" {
				t.Error("no file_url header")
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v26.0/c1":
			if polls.Add(1) < 2 {
				_, _ = w.Write([]byte(`{"status_code":"IN_PROGRESS"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status_code":"FINISHED"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v26.0/ig1/media_publish":
			_, _ = w.Write([]byte(`{"id":"m77"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v26.0/m77":
			_, _ = w.Write([]byte(`{"permalink":"https://www.instagram.com/reel/abc/"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ig := NewInstagramReels(srv.URL + "/v26.0")
	ctx := context.Background()
	acct := Account{ExternalID: "ig1", Token: "tok"}
	v := Video{URL: "https://media.example/v.mp4", Bytes: 1000}
	p := Post{Caption: "Holiday coding", AIGenerated: true}

	st, err := ig.Start(ctx, acct, v, p)
	if err != nil {
		t.Fatal(err)
	}
	if st, err = ig.Upload(ctx, acct, v, st); err != nil {
		t.Fatal(err)
	}
	if st, err = ig.Finish(ctx, acct, p, st); err != nil || st.ExternalID != "m77" {
		t.Fatalf("finish: %+v %v", st, err)
	}
	res, err := ig.Status(ctx, acct, st)
	if err != nil || !res.Published || res.Permalink != "https://www.instagram.com/reel/abc/" {
		t.Fatalf("status: %+v %v", res, err)
	}
}

func TestInstagramQuotaAndContainerError(t *testing.T) {
	igPollEvery, igPollFor = 10*time.Millisecond, time.Second
	defer func() { igPollEvery, igPollFor = time.Minute, 5*time.Minute }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/content_publishing_limit"):
			_, _ = w.Write([]byte(`{"data":[{"quota_usage":50,"config":{"quota_total":50}}]}`))
		default:
			_, _ = w.Write([]byte(`{"status_code":"ERROR"}`))
		}
	}))
	defer srv.Close()
	ig := NewInstagramReels(srv.URL)
	if _, err := ig.Start(context.Background(), Account{ExternalID: "ig1", Token: "t"}, Video{}, Post{}); err == nil || !strings.Contains(err.Error(), "24 hours") {
		t.Fatalf("full quota must stop before upload: %v", err)
	}
	if _, err := ig.Finish(context.Background(), Account{ExternalID: "ig1", Token: "t"}, Post{}, UploadState{ExternalID: "c1"}); err == nil || !strings.Contains(err.Error(), "error") {
		t.Fatalf("container ERROR must fail: %v", err)
	}
}
