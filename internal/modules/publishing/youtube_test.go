package publishing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A fake object store that serves Range requests and a fake YouTube that accepts the
// resumable protocol, so a file of two chunks and a bit goes through three PUTs.
func TestYouTubeResumableUpload(t *testing.T) {
	size := int64(2*ChunkSize + 1000)
	file := make([]byte, size)
	for i := range file {
		file[i] = byte(i % 251)
	}

	var mu sync.Mutex
	var received []byte
	var meta map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/media/v.mp4":
			var from, to int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
				t.Errorf("range %q", r.Header.Get("Range"))
			}
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(file[from : to+1])
		case r.Method == http.MethodPost && r.URL.Path == "/upload/videos":
			if r.URL.Query().Get("uploadType") != "resumable" || r.Header.Get("X-Upload-Content-Length") != strconv.FormatInt(size, 10) {
				t.Errorf("start: %v %v", r.URL.Query(), r.Header)
			}
			_ = json.NewDecoder(r.Body).Decode(&meta)
			w.Header().Set("Location", srv.URL+"/session/1")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.URL.Path == "/session/1":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			received = append(received, b...)
			n := int64(len(received))
			mu.Unlock()
			if n < size {
				w.Header().Set("Range", "bytes=0-"+strconv.FormatInt(n-1, 10))
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"yt42"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/videos":
			_, _ = w.Write([]byte(`{"items":[{"status":{"uploadStatus":"processed"}}]}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	yt := NewYouTubeShorts(srv.URL+"/upload", srv.URL+"/api", "")
	ctx := context.Background()
	acct := Account{Token: "tok"}
	v := Video{URL: srv.URL + "/media/v.mp4", Bytes: size}
	p := Post{Caption: "Holiday coding <for kids>\nBook now", AIGenerated: true}

	st, err := yt.Start(ctx, acct, v, p)
	if err != nil {
		t.Fatal(err)
	}
	snip := meta["snippet"].(map[string]any)
	status := meta["status"].(map[string]any)
	if snip["title"] != "Holiday coding for kids" || status["containsSyntheticMedia"] != true ||
		status["selfDeclaredMadeForKids"] != false || status["privacyStatus"] != "public" {
		t.Fatalf("metadata: %v", meta)
	}
	for calls := 0; st.Offset < size; calls++ {
		if calls > 3 {
			t.Fatal("too many chunks")
		}
		if st, err = yt.Upload(ctx, acct, v, st); err != nil {
			t.Fatal(err)
		}
	}
	if st.ExternalID != "yt42" || st.ChunkIndex != 3 || string(received) != string(file) {
		t.Fatalf("upload: %+v, %d bytes", st, len(received))
	}
	if _, err := yt.Finish(ctx, acct, p, st); err != nil {
		t.Fatal(err)
	}
	res, err := yt.Status(ctx, acct, st)
	if err != nil || !res.Published || res.Permalink != "https://www.youtube.com/shorts/yt42" {
		t.Fatalf("status: %+v %v", res, err)
	}
}

func TestYouTubeResumeAndExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/live":
			if r.Header.Get("Content-Range") != "bytes */100" {
				t.Errorf("resume header %q", r.Header.Get("Content-Range"))
			}
			w.Header().Set("Range", "bytes=0-49")
			w.WriteHeader(http.StatusPermanentRedirect)
		case "/session/gone":
			w.WriteHeader(http.StatusNotFound)
		case "/api/videos":
			_, _ = w.Write([]byte(`{"items":[{"status":{"uploadStatus":"rejected","rejectionReason":"duplicate"}}]}`))
		}
	}))
	defer srv.Close()
	yt := NewYouTubeShorts(srv.URL, srv.URL+"/api", "private")
	v := Video{Bytes: 100}
	st, err := yt.Resume(context.Background(), Account{}, v, UploadState{SessionURI: srv.URL + "/session/live"})
	if err != nil || st.Offset != 50 {
		t.Fatalf("resume: %+v %v", st, err)
	}
	if _, err := yt.Resume(context.Background(), Account{}, v, UploadState{SessionURI: srv.URL + "/session/gone"}); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expiry: %v", err)
	}
	res, err := yt.Status(context.Background(), Account{}, UploadState{ExternalID: "x"})
	if err != nil || !res.Done || res.Published || !strings.Contains(res.Error, "duplicate") {
		t.Fatalf("rejected: %+v %v", res, err)
	}
}

func TestRangeEndAndTitle(t *testing.T) {
	if rangeEnd("bytes=0-8388607") != 8388608 || rangeEnd("") != 0 {
		t.Fatal("rangeEnd")
	}
	if youtubeTitle("") != "New video" || len([]rune(youtubeTitle(strings.Repeat("a", 150)))) != 100 {
		t.Fatal("title")
	}
}
