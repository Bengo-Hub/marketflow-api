package contacts

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
)

// OptedOutKey is the contact and lead metadata key holding when the person opted out, per channel
// ({"email": time, "phone": time}). The consent flags start off, so an off flag alone never says
// that the person asked to be left alone.
const OptedOutKey = "opted_out"

const markOptedOut = ` SET metadata = (CASE WHEN jsonb_typeof(metadata) = 'object' THEN metadata ELSE '{}'::jsonb END) ||
	jsonb_build_object('` + OptedOutKey + `', COALESCE(CASE WHEN jsonb_typeof(metadata->'` + OptedOutKey + `') = 'object' THEN metadata->'` + OptedOutKey + `' END, '{}'::jsonb)
		|| jsonb_build_object($3::text, $4::text))
	WHERE tenant_id = $1 AND `

// MarkOptedOut records an opt-out on the person's contacts and leads: channel is "email" with the
// normalised address, or "phone" with its last nine digits.
func MarkOptedOut(ctx context.Context, db *ent.Client, tenantID uuid.UUID, channel, key, at string) error {
	match := map[string][2]string{
		"email": {"email_normalized = $2", "LOWER(TRIM(email)) = $2"},
		"phone": {"phone_subscriber_digits = $2", "RIGHT(REGEXP_REPLACE(phone, '[^0-9]', '', 'g'), 9) = $2"},
	}[channel]
	for i, table := range []string{"contacts", "leads"} {
		if _, err := db.ExecContext(ctx, "UPDATE "+table+markOptedOut+match[i], tenantID, key, channel, at); err != nil {
			return err
		}
	}
	return nil
}

// PersonContacts are the tenant's contacts with this email (any case) or phone (last nine digits),
// oldest first.
func PersonContacts(ctx context.Context, db *ent.Client, tenantID uuid.UUID, email, phoneDigits string) ([]*ent.Contact, error) {
	q := db.Contact.Query().Where(contact.TenantID(tenantID)).Order(ent.Asc(contact.FieldCreatedAt), ent.Asc(contact.FieldID)).Limit(20)
	email = strings.ToLower(strings.TrimSpace(email))
	switch {
	case email != "" && len(phoneDigits) == 9:
		q = q.Where(contact.Or(contact.EmailNormalized(email), contact.PhoneSubscriberDigits(phoneDigits)))
	case email != "":
		q = q.Where(contact.EmailNormalized(email))
	case len(phoneDigits) == 9:
		q = q.Where(contact.PhoneSubscriberDigits(phoneDigits))
	default:
		return nil, nil
	}
	return q.All(ctx)
}

// OptedOut reports whether the person asked not to be contacted: a recorded opt-out for a channel
// no contact of theirs is switched on for again, or (from before opt-outs were recorded) a consent
// that was switched on and is off for every channel now. metas are their leads' metadata.
func OptedOut(people []*ent.Contact, metas ...map[string]any) bool {
	var emailOn, phoneOn bool
	for _, c := range people {
		emailOn, phoneOn = emailOn || c.SubscribedEmail, phoneOn || c.SubscribedSms
		metas = append(metas, c.Metadata)
	}
	for _, m := range metas {
		out, _ := m[OptedOutKey].(map[string]any)
		if (out["email"] != nil && !emailOn) || (out["phone"] != nil && !phoneOn) {
			return true
		}
	}
	for _, c := range people {
		// These sources never switched a flag on.
		src, _ := c.Metadata["consent_source"].(string)
		if src != "" && src != "legacy_unverified" && src != "meta_lead_form" && !emailOn && !phoneOn {
			return true
		}
	}
	return false
}
