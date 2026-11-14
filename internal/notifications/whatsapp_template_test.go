package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A template send carries notifications-api's template keys, with parameters made safe for Meta
// (no newlines), and wins over a rich form.
func TestWhatsAppTemplateMetadata(t *testing.T) {
	m := metaOf(t, Message{Channel: ChannelWhatsApp, Recipient: "+254700000001", Body: "Your video ad is ready.",
		Template: &WhatsAppTemplate{Name: "mf_video_ready_v1_btn", Params: []string{"Boi Salon", "Weekend\noffer  now"}, ButtonParam: "boi/studio/b1"},
		WhatsApp: &WhatsAppRich{CTAText: "Open", CTAURL: "https://mf.example"}, PlatformDialCode: "254"})
	params, _ := m["template_params"].([]any)
	if m["template_name"] != "mf_video_ready_v1_btn" || m["template_language"] != "en_US" || len(params) != 2 ||
		params[1] != "Weekend offer now" || m["template_button_param"] != "boi/studio/b1" || m["default_dial_code"] != "254" {
		t.Fatalf("template metadata: %v", m)
	}
	if _, ok := m["cta_button_text"]; ok {
		t.Fatal("a template send must not also carry the rich form")
	}
	// The one-time code template: the code is the body parameter and the copy button's value.
	m = metaOf(t, Message{Channel: ChannelWhatsApp, Recipient: "+254700000001", Body: "Your code",
		Template: &WhatsAppTemplate{Name: "auth_otp", Params: []string{"482915"}, ButtonParam: "482915"}})
	if m["template_name"] != "auth_otp" || m["template_button_param"] != "482915" {
		t.Fatalf("otp metadata: %v", m)
	}
}

func TestTemplateStatuses(t *testing.T) {
	var gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotQuery = r.Header.Get("X-API-Key"), r.URL.RawQuery
		if r.URL.Path != "/api/v1/s2s/whatsapp/templates/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"tenant_id":"plat","templates":[{"name":"mf_video_ready_v1_btn","status":"APPROVED"},{"name":"mf_daily_brief_v1_btn","status":"PENDING"}]}`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, APIKey: "svc"})
	got, err := c.TemplateStatuses(context.Background(), "plat", []string{"mf_video_ready_v1_btn", "mf_daily_brief_v1_btn"})
	if err != nil || got["mf_video_ready_v1_btn"].Status != "APPROVED" || got["mf_daily_brief_v1_btn"].Status != "PENDING" {
		t.Fatalf("statuses: %+v %v", got, err)
	}
	if gotKey != "svc" || gotQuery != "names=mf_video_ready_v1_btn%2Cmf_daily_brief_v1_btn&tenant_id=plat" {
		t.Fatalf("request: %q %q", gotKey, gotQuery)
	}
	if _, err := New(Config{BaseURL: srv.URL}).TemplateStatuses(context.Background(), "plat", []string{"x"}); err == nil {
		t.Fatal("no service key must be an error")
	}
}
