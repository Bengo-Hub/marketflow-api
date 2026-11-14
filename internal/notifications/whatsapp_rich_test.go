package notifications

import (
	"encoding/json"
	"testing"
)

// metaOf builds the wire request and returns its metadata after a JSON round trip, as
// notifications-api reads it.
func metaOf(t *testing.T, msg Message) map[string]any {
	t.Helper()
	req, err := toRequest("tenant-1", msg)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req)
	var wire struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.Metadata
}

func TestWhatsAppRichButtons(t *testing.T) {
	m := metaOf(t, Message{Channel: ChannelWhatsApp, Recipient: "+254700000001", Body: "Here is the plan.",
		WhatsApp: &WhatsAppRich{Buttons: []WhatsAppButton{{ID: "mf:yes:r1:4821", Title: "Approve"}, {ID: "mf:cancel:r1", Title: "Cancel"}},
			FallbackText: "Reply YES 4821 to approve"}})
	btns, _ := m["interactive_buttons"].([]any)
	if len(btns) != 2 || btns[0].(map[string]any)["id"] != "mf:yes:r1:4821" || m["fallback_text"] != "Reply YES 4821 to approve" {
		t.Fatalf("buttons metadata: %v", m)
	}
	if _, ok := m["media_type"]; ok {
		t.Fatal("a button message has no media")
	}
}

func TestWhatsAppRichListMediaCTA(t *testing.T) {
	m := metaOf(t, Message{Channel: ChannelWhatsApp, Recipient: "+254700000001", Body: "Which business?",
		WhatsApp: &WhatsAppRich{List: &WhatsAppList{Button: "Businesses", Rows: []WhatsAppListRow{{ID: "mf:t:1", Title: "Boi Salon"}}}}})
	rows, _ := m["interactive_list_rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["title"] != "Boi Salon" || m["interactive_list_button"] != "Businesses" {
		t.Fatalf("list metadata: %v", m)
	}

	m = metaOf(t, Message{Channel: ChannelWhatsApp, Recipient: "+254700000001", Body: "Scene 1 of 3",
		WhatsApp: &WhatsAppRich{MediaType: "image", MediaLink: "https://media.example/s1.jpg"}})
	if m["media_type"] != "image" || m["media_link"] != "https://media.example/s1.jpg" {
		t.Fatalf("media metadata: %v", m)
	}

	m = metaOf(t, Message{Channel: ChannelWhatsApp, Recipient: "+254700000001", Body: "Sign up to use MarketFlow here.",
		WhatsApp: &WhatsAppRich{CTAText: "Sign up", CTAURL: "https://mf.example"}})
	if m["cta_button_text"] != "Sign up" || m["cta_button_url"] != "https://mf.example" {
		t.Fatalf("cta metadata: %v", m)
	}

	// Another channel ignores it.
	m = metaOf(t, Message{Channel: ChannelSMS, Recipient: "+254700000001", Body: "hi",
		WhatsApp: &WhatsAppRich{MediaType: "image", MediaLink: "https://media.example/s1.jpg"}})
	if _, ok := m["media_link"]; ok {
		t.Fatalf("sms must not carry whatsapp keys: %v", m)
	}
}
