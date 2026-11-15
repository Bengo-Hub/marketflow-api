package inbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
)

// The private reply goes to the receiving account's /messages edge with the comment as the
// recipient, as Meta documents it for both Facebook and Instagram.
func TestGraphPrivateReply(t *testing.T) {
	var path, token string
	var body map[string]map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, token = r.URL.Path, r.URL.Query().Get("access_token")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = w.Write([]byte(`{"recipient_id":"R1","message_id":"m_1"}`))
	}))
	defer srv.Close()
	g := NewGraph(srv.URL + "/v26.0")
	id, err := g.PrivateReply(context.Background(), "PAGE1", "page-token", "123_456", "Prices here: https://x.test/p")
	if err != nil || id != "m_1" {
		t.Fatalf("id %q err %v", id, err)
	}
	if path != "/v26.0/PAGE1/messages" || token != "page-token" || body["recipient"]["comment_id"] != "123_456" ||
		!strings.HasPrefix(body["message"]["text"], "Prices here") {
		t.Fatalf("path %s token %s body %v", path, token, body)
	}
	if _, err := g.PrivateReply(context.Background(), "", "t", "c", "x"); err == nil {
		t.Fatal("no account id must fail before any call")
	}
}

// One private reply per comment, within 7 days, on a live Facebook or Instagram comment only.
func TestPrivateReplyWindow(t *testing.T) {
	now := time.Now()
	ref := "123_456"
	item := func(kind, platform string, at time.Time, meta map[string]any) *ent.Activity {
		m := map[string]any{"platform": platform}
		for k, v := range meta {
			m[k] = v
		}
		return &ent.Activity{ID: uuid.New(), ActivityType: kind, ExternalRef: &ref, PerformedAt: at, Metadata: m}
	}
	cases := []struct {
		name string
		a    *ent.Activity
		ok   bool
		why  string
	}{
		{"fresh facebook comment", item("comment", "facebook", now.Add(-time.Hour), nil), true, ""},
		{"instagram comment at six days", item("comment", "instagram", now.Add(-6*24*time.Hour), nil), true, ""},
		{"eight days old", item("comment", "facebook", now.Add(-8*24*time.Hour), nil), false, "7 days"},
		{"already sent", item("comment", "facebook", now, map[string]any{"private_replied_at": now.Format(time.RFC3339)}), false, "already"},
		{"deleted", item("comment", "facebook", now, map[string]any{"deleted_at": now.Format(time.RFC3339)}), false, "deleted"},
		{"a dm", item("dm", "facebook", now, nil), false, ""},
		{"whatsapp", item("comment", "whatsapp", now, nil), false, ""},
	}
	for _, c := range cases {
		ok, why := privateReplyWindow(c.a, now)
		if ok != c.ok || (c.why != "" && !strings.Contains(why, c.why)) {
			t.Errorf("%s: ok %v why %q", c.name, ok, why)
		}
	}
	it := toItem(item("comment", "facebook", now, nil), now)
	if !it.CanPrivateReply || it.PrivateReplyWindow != "" {
		t.Fatalf("item %+v", it)
	}
}
