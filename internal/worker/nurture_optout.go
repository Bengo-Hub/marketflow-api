package worker

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
)

// nurtureChannelAfterOptOut decides where a nurture step may go once the person's opt-outs are
// known (Sprint 23 flag, decided 2026-11-10 while the owner was away): a step on an opted-out
// channel goes by email when the person has an email they did not opt out of; otherwise it is
// not sent. SMS and WhatsApp share one phone opt-out, as STOP sets it.
func nurtureChannelAfterOptOut(channel, recipient, email string, emailOut, phoneOut bool) (string, string, bool) {
	switch channel {
	case "sms", "whatsapp":
		if !phoneOut {
			return channel, recipient, true
		}
		if email != "" && !emailOut {
			return "email", email, true
		}
		return "", "", false
	default:
		if emailOut {
			return "", "", false
		}
		return channel, recipient, true
	}
}

// contactOptOuts reports whether any of the tenant's contacts with this email or phone opted out
// of email or of SMS/WhatsApp, matched the way ApplyUnsubscribe sets them (normalised email; a
// phone's last nine digits).
func contactOptOuts(ctx context.Context, db *ent.Client, tenantID uuid.UUID, email, phone string) (emailOut, phoneOut bool) {
	if e := strings.ToLower(strings.TrimSpace(email)); e != "" {
		n, err := db.Contact.Query().
			Where(contact.TenantID(tenantID), contact.EmailNormalized(e), contact.SubscribedEmail(false)).
			Count(ctx)
		emailOut = err == nil && n > 0
	}
	if d := lastNine(phone); len(d) == 9 {
		n, err := db.Contact.Query().
			Where(contact.TenantID(tenantID), contact.PhoneSubscriberDigits(d), contact.SubscribedSms(false)).
			Count(ctx)
		phoneOut = err == nil && n > 0
	}
	return emailOut, phoneOut
}

func lastNine(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) > 9 {
		d = d[len(d)-9:]
	}
	return d
}
