package webhooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"
)

func tiktokSign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(body)
	return "t=" + strconv.FormatInt(ts, 10) + ",s=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyTikTokSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"event":"lead"}`)
	cases := []struct {
		name   string
		header string
		secret string
		want   bool
	}{
		{"valid", tiktokSign("k", now.Unix(), body), "k", true},
		{"valid, spaces", " t=" + strconv.FormatInt(now.Unix(), 10) + " , s=" + tiktokSign("k", now.Unix(), body)[len("t=1800000000,s="):], "k", true},
		{"wrong key", tiktokSign("other", now.Unix(), body), "k", false},
		{"other body", tiktokSign("k", now.Unix(), []byte(`{}`)), "k", false},
		{"too old", tiktokSign("k", now.Add(-10*time.Minute).Unix(), body), "k", false},
		{"from the future", tiktokSign("k", now.Add(10*time.Minute).Unix(), body), "k", false},
		{"no timestamp", "s=abc", "k", false},
		{"garbage", "nonsense", "k", false},
		{"no secret", tiktokSign("", now.Unix(), body), "", false},
	}
	for _, c := range cases {
		if got := verifyTikTokSignature(c.header, body, c.secret, now); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestTikTokAuthorised(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"event":"lead"}`)
	cases := []struct {
		name             string
		shared, signing  string
		allowOpen        bool
		query, sigHeader string
		want             bool
	}{
		{"signature wins", "s3", "app", false, "", tiktokSign("app", now.Unix(), body), true},
		{"bad signature is refused even with the right query secret", "s3", "app", false, "?secret=s3", tiktokSign("x", now.Unix(), body), false},
		{"no header falls back to the query", "s3", "app", false, "?secret=s3", "", true},
		{"wrong query secret", "s3", "app", false, "?secret=no", "", false},
		{"header without a signing key uses the query", "s3", "", false, "?secret=s3", "t=1,s=2", true},
		{"nothing configured is closed", "", "", false, "", "", false},
		{"development may run open", "", "", true, "", "", true},
		{"signature alone when no query secret is set", "", "app", false, "", tiktokSign("app", now.Unix(), body), true},
	}
	for _, c := range cases {
		h := &TikTokHandler{sharedSecret: c.shared, signingSecret: c.signing, allowOpen: c.allowOpen, logger: zap.NewNop(), now: func() time.Time { return now }}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/tiktok/leads"+c.query, bytes.NewReader(body))
		if c.sigHeader != "" {
			r.Header.Set(tiktokSignatureHeader, c.sigHeader)
		}
		if got, _ := h.authorised(r, body); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

// Audit M3: webhook bodies over 1 MB are refused before they are read in full.
func TestWebhookBodyLimit(t *testing.T) {
	big := bytes.Repeat([]byte("a"), maxWebhookBody+1)
	cases := []struct {
		name  string
		serve func(w http.ResponseWriter, r *http.Request)
	}{
		{"tiktok", (&TikTokHandler{sharedSecret: "s", logger: zap.NewNop(), now: time.Now}).HandleLeads},
		{"meta", (&MetaHandler{logger: zap.NewNop()}).HandleLeadgen},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		c.serve(w, httptest.NewRequest(http.MethodPost, "/hook?secret=s", bytes.NewReader(big)))
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: status %d", c.name, w.Code)
		}
	}
}
