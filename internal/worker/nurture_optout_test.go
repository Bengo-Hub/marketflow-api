package worker

import "testing"

// Sprint 23 flag (2026-11-10): nurture never read the contact opt-outs that STOP and unsubscribe
// links set, so an opted-out person kept getting sequence messages. A step on an opted-out
// channel falls back to email when email is still allowed; otherwise nothing is sent.
func TestNurtureChannelAfterOptOut(t *testing.T) {
	cases := []struct {
		name                   string
		channel, email, phone  string
		emailOut, phoneOut     bool
		wantCh, wantTo         string
		wantSend               bool
	}{
		{"email allowed", "email", "a@b.c", "", false, false, "email", "a@b.c", true},
		{"email opted out", "email", "a@b.c", "+254700000001", true, false, "", "", false},
		{"sms allowed", "sms", "a@b.c", "+254700000001", false, false, "sms", "+254700000001", true},
		{"sms opted out, email ok", "sms", "a@b.c", "+254700000001", false, true, "email", "a@b.c", true},
		{"whatsapp opted out, no email", "whatsapp", "", "+254700000001", false, true, "", "", false},
		{"both opted out", "sms", "a@b.c", "+254700000001", true, true, "", "", false},
	}
	for _, c := range cases {
		recipient := c.email
		if c.channel != "email" {
			recipient = c.phone
		}
		ch, to, send := nurtureChannelAfterOptOut(c.channel, recipient, c.email, c.emailOut, c.phoneOut)
		if ch != c.wantCh || to != c.wantTo || send != c.wantSend {
			t.Errorf("%s: got %s %s %v", c.name, ch, to, send)
		}
	}
}
