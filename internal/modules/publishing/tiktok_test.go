package publishing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTikTokChunks(t *testing.T) {
	cases := []struct {
		size  int64
		chunk int64
		count int
	}{
		{1000, 1000, 1},
		{ChunkSize - 1, ChunkSize - 1, 1},
		{ChunkSize, ChunkSize, 1},
		{2*ChunkSize + 5, ChunkSize, 2}, // the 5 bytes ride on chunk 2
	}
	for _, c := range cases {
		chunk, count := tiktokChunks(c.size)
		if chunk != c.chunk || count != c.count {
			t.Errorf("size %d: got %d x %d", c.size, chunk, count)
		}
	}
	if tiktokMinChunk > ChunkSize {
		t.Fatal("ChunkSize is under TikTok's 5 MB minimum")
	}
}

func TestTikTokFileUploadFlow(t *testing.T) {
	size := int64(2*ChunkSize + 5)
	file := make([]byte, size)
	for i := range file {
		file[i] = byte(i % 241)
	}
	var got []byte
	var init map[string]map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/post/publish/creator_info/query/":
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Error("no bearer")
			}
			_, _ = w.Write([]byte(`{"data":{"privacy_level_options":["SELF_ONLY","PUBLIC_TO_EVERYONE"],"duet_disabled":true,"max_video_post_duration_sec":600},"error":{"code":"ok"}}`))
		case "/v2/post/publish/video/init/":
			_ = json.NewDecoder(r.Body).Decode(&init)
			_, _ = w.Write([]byte(`{"data":{"publish_id":"p_1","upload_url":"` + srv.URL + `/up"},"error":{"code":"ok"}}`))
		case "/media":
			var from, to int64
			_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(file[from : to+1])
		case "/up":
			b, _ := io.ReadAll(r.Body)
			want := fmt.Sprintf("bytes %d-%d/%d", len(got), len(got)+len(b)-1, size)
			if r.Header.Get("Content-Range") != want {
				t.Errorf("content-range %q, want %q", r.Header.Get("Content-Range"), want)
			}
			got = append(got, b...)
			if int64(len(got)) == size {
				w.WriteHeader(http.StatusCreated)
				return
			}
			w.WriteHeader(http.StatusPartialContent)
		case "/v2/post/publish/status/fetch/":
			_, _ = w.Write([]byte(`{"data":{"status":"PUBLISH_COMPLETE","publicaly_available_post_id":["7300"]},"error":{"code":"ok"}}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	tt := NewTikTokDirect(srv.URL, "", false)
	ctx := context.Background()
	acct := Account{Token: "tok"}
	v := Video{URL: srv.URL + "/media", Bytes: size, DurationMS: 30000}
	p := Post{Caption: "Holiday coding", AIGenerated: true}
	st, err := tt.Start(ctx, acct, v, p)
	if err != nil {
		t.Fatal(err)
	}
	pi, si := init["post_info"], init["source_info"]
	if pi["privacy_level"] != "SELF_ONLY" || pi["is_aigc"] != true || pi["disable_duet"] != true ||
		si["source"] != "FILE_UPLOAD" || si["total_chunk_count"] != float64(2) {
		t.Fatalf("init body: %v", init)
	}
	for i := 0; st.Offset < size; i++ {
		if i > 2 {
			t.Fatal("too many chunks")
		}
		if st, err = tt.Upload(ctx, acct, v, st); err != nil {
			t.Fatal(err)
		}
	}
	if string(got) != string(file) || st.ChunkIndex != 2 {
		t.Fatalf("uploaded %d bytes, state %+v", len(got), st)
	}
	res, err := tt.Status(ctx, acct, st)
	if err != nil || !res.Published || res.Permalink != "https://www.tiktok.com/video/7300" {
		t.Fatalf("status: %+v %v", res, err)
	}
}

func TestTikTokRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/post/publish/creator_info/query/":
			_, _ = w.Write([]byte(`{"data":{"privacy_level_options":["SELF_ONLY"],"max_video_post_duration_sec":60},"error":{"code":"ok"}}`))
		case "/v2/post/publish/status/fetch/":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"access_token_invalid","message":"The access token is invalid"}}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if _, err := NewTikTokDirect(srv.URL, "PUBLIC_TO_EVERYONE", true).Start(ctx, Account{}, Video{DurationMS: 1000}, Post{}); err == nil || !strings.Contains(err.Error(), "privacy") {
		t.Fatalf("public before audit must fail: %v", err)
	}
	if _, err := NewTikTokDirect(srv.URL, "", true).Start(ctx, Account{}, Video{DurationMS: 90000}, Post{}); err == nil || !strings.Contains(err.Error(), "60 seconds") {
		t.Fatalf("too long must fail: %v", err)
	}
	if _, err := NewTikTokDirect(srv.URL, "", true).Status(ctx, Account{}, UploadState{ExternalID: "p"}); err == nil || !strings.Contains(err.Error(), "access_token_invalid") {
		t.Fatalf("api error not surfaced: %v", err)
	}
}
