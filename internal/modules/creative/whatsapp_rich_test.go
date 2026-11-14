package creative

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/notifications"
)

func TestWhatsAppConfigFromEnv(t *testing.T) {
	cfg, err := WhatsAppConfigFromEnv(func(string) string { return "" })
	if err != nil || cfg != DefaultWhatsAppConfig() {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	env := map[string]string{"MF_WHATSAPP_WINDOW": "23h", "MF_WHATSAPP_VIDEO_MAX_BYTES": "1000", "MF_WHATSAPP_IMAGE_MAX_BYTES": "nope"}
	cfg, err = WhatsAppConfigFromEnv(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "MF_WHATSAPP_IMAGE_MAX_BYTES") {
		t.Fatalf("a bad value must be reported: %v", err)
	}
	if cfg.Window != 23*time.Hour || cfg.VideoMaxBytes != 1000 || cfg.ImageMaxBytes != DefaultWhatsAppConfig().ImageMaxBytes {
		t.Fatalf("parsed: %+v", cfg)
	}
	if _, err := WhatsAppConfigFromEnv(func(k string) string {
		if k == "MF_WHATSAPP_WINDOW" {
			return "30h"
		}
		return ""
	}); err == nil {
		t.Fatal("a window past Meta's 24 hours must be refused")
	}
}

func TestMediaFits(t *testing.T) {
	cfg := DefaultWhatsAppConfig()
	cases := []struct {
		kind, ct string
		size     int64
		want     bool
	}{
		{"video", "video/mp4", 15 << 20, true},
		{"video", "video/mp4", 17 << 20, false}, // over 16 MB: the link goes instead
		{"video", "", -1, false},                // unknown size is never sent natively
		{"video", "application/octet-stream", 1 << 20, false},
		{"image", "image/jpeg", 1 << 20, true},
		{"image", "image/png", 6 << 20, false},
		{"document", "application/pdf", 1 << 10, false},
	}
	for _, tc := range cases {
		if got := MediaFits(tc.kind, tc.ct, tc.size, cfg); got != tc.want {
			t.Fatalf("MediaFits(%s, %s, %d) = %v, want %v", tc.kind, tc.ct, tc.size, got, tc.want)
		}
	}
	if contentRangeTotal("bytes 0-0/12345") != 12345 || contentRangeTotal("bytes 0-0/*") != -1 || contentRangeTotal("") != -1 {
		t.Fatal("content-range total")
	}
}

// The size check asks for one byte and reads the total, so signed GET links work.
func TestMediaFitsProbe(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" {
			t.Errorf("range header %q", r.Header.Get("Range"))
		}
		switch r.URL.Path {
		case "/small.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 0-0/2000000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
		case "/big.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 0-0/40000000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	p := NewPlatformWhatsApp(nil, nil, "codevertex", "test").WithRich(nil, WhatsAppConfig{})
	p.probe = srv.Client()
	ctx := context.Background()
	if !p.mediaFits(ctx, notifications.WhatsAppMedia{Type: "video", Link: srv.URL + "/small.mp4"}) {
		t.Fatal("a 2 MB mp4 goes natively")
	}
	if p.mediaFits(ctx, notifications.WhatsAppMedia{Type: "video", Link: srv.URL + "/big.mp4"}) {
		t.Fatal("a 40 MB mp4 goes as a link")
	}
	if p.mediaFits(ctx, notifications.WhatsAppMedia{Type: "video", Link: srv.URL + "/denied.mp4"}) {
		t.Fatal("a refused probe goes as a link")
	}
	if p.mediaFits(ctx, notifications.WhatsAppMedia{Type: "image", Link: "http://insecure.example/s.jpg"}) {
		t.Fatal("only https links are sent natively")
	}
}

// Without Redis the window counts as closed, so everything goes as plain text.
func TestWindowClosedWithoutRedis(t *testing.T) {
	p := NewPlatformWhatsApp(nil, nil, "codevertex", "test")
	if p.InWindow(context.Background(), "+254700000001") {
		t.Fatal("no redis means no rich sends")
	}
	if p.Window() != DefaultWhatsAppConfig().Window {
		t.Fatalf("window = %v", p.Window())
	}
	if whatsAppWindowKey("+254700000001") != whatsAppWindowKey("254700000001") {
		t.Fatal("the window key must not depend on the plus sign")
	}
	// No notifications client: nothing is sent and nothing fails.
	if err := p.SendRich(context.Background(), "+254700000001", "text", &notifications.WhatsAppInteractive{Body: "b"}, "k"); err != nil {
		t.Fatal(err)
	}
}
