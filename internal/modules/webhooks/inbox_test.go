package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestParseInboxPage(t *testing.T) {
	body := []byte(`{"object":"page","entry":[{"id":"PAGE1","time":1762240000000,
	  "changes":[
	    {"field":"feed","value":{"item":"comment","verb":"add","comment_id":"C1","post_id":"PAGE1_P9","message":"How much is this?","created_time":1762240000,"from":{"id":"U1","name":"Amina"}}},
	    {"field":"feed","value":{"item":"comment","verb":"add","comment_id":"C2","post_id":"PAGE1_P9","message":"Thanks!","from":{"id":"PAGE1","name":"Our Page"}}},
	    {"field":"feed","value":{"item":"reaction","verb":"add","post_id":"PAGE1_P9","from":{"id":"U2"}}},
	    {"field":"leadgen","value":{"leadgen_id":"L1"}}],
	  "messaging":[
	    {"sender":{"id":"PSID7"},"recipient":{"id":"PAGE1"},"timestamp":1762240001000,"message":{"mid":"m_1","text":"Do you ship to Nakuru?"}},
	    {"sender":{"id":"PAGE1"},"recipient":{"id":"PSID7"},"timestamp":1762240002000,"message":{"mid":"m_2","text":"Yes","is_echo":true}},
	    {"sender":{"id":"PSID8"},"recipient":{"id":"PAGE1"},"timestamp":1762240003000,"message":{"mid":"m_3"}}]}]}`)
	items := ParseInbox(body)
	if len(items) != 2 {
		t.Fatalf("want the user comment and the user DM only, got %+v", items)
	}
	c, d := items[0], items[1]
	if c.Kind != "comment" || c.ExternalID != "C1" || c.PostID != "PAGE1_P9" || c.SenderName != "Amina" || c.Platform != "facebook" {
		t.Fatalf("comment: %+v", c)
	}
	if d.Kind != "dm" || d.ExternalID != "m_1" || d.SenderID != "PSID7" || d.At.UnixMilli() != 1762240001000 {
		t.Fatalf("dm: %+v", d)
	}
}

func TestParseInboxInstagramBothShapes(t *testing.T) {
	// Facebook Login for Business: changes[] with comment_id; Business Login: field/value on the entry with id.
	body := []byte(`{"object":"instagram","entry":[
	  {"id":"IG1","time":1762240000,"changes":[{"field":"comments","value":{"comment_id":"IC1","text":"price?","from":{"id":"IGU1","username":"wanjiku"},"media":{"id":"MED1","ad_id":"AD5"}}}]},
	  {"id":"IG1","time":1762240000,"field":"comments","value":{"id":"IC2","text":"where are you?","from":{"id":"IGU2","username":"otieno"},"media":{"id":"MED2"}}},
	  {"id":"IG1","time":1762240000,"changes":[{"field":"comments","value":{"comment_id":"IC3","text":"thank you all","from":{"id":"IG1","username":"us"},"media":{"id":"MED1"}}}]}]}`)
	items := ParseInbox(body)
	if len(items) != 2 || items[0].ExternalID != "IC1" || items[0].AdID != "AD5" || items[1].ExternalID != "IC2" || items[1].Platform != "instagram" {
		t.Fatalf("instagram: %+v", items)
	}
	if ParseInbox([]byte(`{"object":"whatsapp_business_account","entry":[]}`)) != nil {
		t.Fatal("other objects are not inbox events")
	}
}

func TestMetaWebhookFailsClosedWithoutSecret(t *testing.T) {
	body := []byte(`{"object":"page","entry":[]}`)
	h := NewMetaHandler(nil, MetaConfig{}, zap.NewNop())
	rec := httptest.NewRecorder()
	h.HandleLeadgen(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no secret must refuse, got %d", rec.Code)
	}

	// The secret resolver (provider row first) is used, and a good signature passes.
	h = NewMetaHandler(nil, MetaConfig{Secret: func(context.Context) string { return "s3cret" }}, zap.NewNop())
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec = httptest.NewRecorder()
	h.HandleLeadgen(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a signed event must pass, got %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=00")
	rec = httptest.NewRecorder()
	h.HandleLeadgen(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a bad signature must refuse, got %d", rec.Code)
	}

	h = NewMetaHandler(nil, MetaConfig{AllowUnsigned: true}, zap.NewNop())
	rec = httptest.NewRecorder()
	h.HandleLeadgen(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("development may run unsigned, got %d", rec.Code)
	}
}
