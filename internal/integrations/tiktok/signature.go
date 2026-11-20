package tiktok

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries "t=<unix seconds>,s=<hex HMAC-SHA256 of "<t>.<raw body>">", keyed with
// the app's client secret (TikTok webhooks verification).
const SignatureHeader = "TikTok-Signature"

// SignatureSkew is the largest clock difference accepted, so an old signed event cannot be
// replayed later.
const SignatureSkew = 5 * time.Minute

// VerifySignature checks a TikTok-Signature header against the raw body.
func VerifySignature(header string, body []byte, secret string, now time.Time) bool {
	if secret == "" || header == "" {
		return false
	}
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "s":
			sig = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > SignatureSkew || d < -SignatureSkew {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.ToLower(sig)))
}
