package publishing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Sprint 16: TikTok pulls a video only from the prefix verified in its developer portal.
func TestTikTokCanPull(t *testing.T) {
	tt := NewTikTokDirect("https://open.tiktokapis.com", "", true)
	cases := map[string]bool{
		"https://media.codevertexafrica.com/marketflow/t1/video.mp4":       true,
		"https://MEDIA.codevertexafrica.com/marketflow/t1/video.mp4":       true,
		"http://media.codevertexafrica.com/marketflow/t1/video.mp4":        false,
		"https://media.codevertexafrica.com/other/video.mp4":               false,
		"https://media.codevertexafrica.com/marketflow/../other/video.mp4": false,
		"https://media.codevertexafrica.com.evil.example/marketflow/v.mp4": false,
		"https://user@media.codevertexafrica.com/marketflow/v.mp4":         false,
		"https://v3.fal.media/files/abc/video.mp4":                         false,
	}
	cases[""] = false
	for u, want := range cases {
		if got := tt.canPull(u); got != want {
			t.Errorf("canPull(%q) = %t, want %t", u, got, want)
		}
	}
	if NewTikTokDirect("x", "", false).canPull("https://media.codevertexafrica.com/marketflow/v.mp4") {
		t.Error("pulling switched off must never pull")
	}
	custom := NewTikTokDirect("x", "", true).WithPullPrefix("https://cdn.example.com/mf/")
	if !custom.canPull("https://cdn.example.com/mf/v.mp4") || custom.canPull("https://media.codevertexafrica.com/marketflow/v.mp4") {
		t.Error("WithPullPrefix replaces the verified prefix")
	}
	if NewTikTokDirect("x", "", true).WithPullPrefix("  ").PullPrefix != DefaultTikTokPullPrefix {
		t.Error("an empty override keeps the default prefix")
	}
}

// An unverified prefix answers url_ownership_unverified; the adapter sends the bytes instead.
func TestTikTokPullFallsBackToFileUpload(t *testing.T) {
	var sources []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/post/publish/creator_info/query/":
			_, _ = w.Write([]byte(`{"data":{"privacy_level_options":["SELF_ONLY"]},"error":{"code":"ok"}}`))
		case "/v2/post/publish/video/init/":
			var body map[string]map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			src, _ := body["source_info"]["source"].(string)
			sources = append(sources, src)
			if src == "PULL_FROM_URL" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"url_ownership_unverified","message":"verify the URL prefix"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"publish_id":"p_2","upload_url":"` + srv.URL + `/up"},"error":{"code":"ok"}}`))
		}
	}))
	defer srv.Close()

	tt := NewTikTokDirect(srv.URL, "", true)
	v := Video{URL: DefaultTikTokPullPrefix + "t1/v.mp4", Bytes: 1000, DurationMS: 1000}
	st, err := tt.Start(context.Background(), Account{Token: "tok"}, v, Post{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0] != "PULL_FROM_URL" || sources[1] != "FILE_UPLOAD" {
		t.Fatalf("init sources %v", sources)
	}
	if st.SessionURI != srv.URL+"/up" || st.Offset != 0 {
		t.Fatalf("the fallback uploads the file: %+v", st)
	}

	// A provider-hosted link never tries to pull.
	sources = nil
	v.URL = srv.URL + "/media"
	if _, err := tt.Start(context.Background(), Account{Token: "tok"}, v, Post{}); err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0] != "FILE_UPLOAD" {
		t.Fatalf("an off-prefix video goes as FILE_UPLOAD: %v", sources)
	}
}

// A pulled video has no upload URL; Upload marks it complete without sending bytes.
func TestTikTokPulledUploadIsDone(t *testing.T) {
	tt := NewTikTokDirect("x", "", true)
	st, err := tt.Upload(context.Background(), Account{}, Video{Bytes: 500}, UploadState{ExternalID: "p"})
	if err != nil || st.Offset != 500 {
		t.Fatalf("pulled upload: %+v %v", st, err)
	}
}
