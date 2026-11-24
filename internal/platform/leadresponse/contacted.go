package leadresponse

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

// StatusNew is the status of a lead nobody has contacted yet.
const StatusNew = "new"

// contactTypes are the activity types that reach a person.
var contactTypes = map[string]bool{
	"call": true, "email": true, "sms": true, "whatsapp": true, "meeting": true, "dm": true, "comment": true,
}

// CountsAsContact reports whether an activity a person on the team logged or sent is a contact
// with the lead: an outbound (or undirected) call, email, message or meeting by a person.
// Automated sends (no performed_by) and inbound messages are not.
func CountsAsContact(activityType string, direction *string, performedBy *uuid.UUID) bool {
	if performedBy == nil || *performedBy == uuid.Nil {
		return false
	}
	if direction != nil && *direction == "inbound" {
		return false
	}
	return contactTypes[activityType]
}

// IsReply reports whether an activity is the lead writing or calling in: an inbound call, email
// or message. A repeat form submission is not a reply.
func IsReply(activityType, direction string) bool {
	return direction == "inbound" && contactTypes[activityType] && activityType != "meeting"
}

// MarkContacted moves a lead that is still new to contacted, with contacted_at, in one
// conditional UPDATE. It reports whether the lead changed; a lead past new is left alone.
func MarkContacted(ctx context.Context, db *ent.Client, tenantID, leadID uuid.UUID, at time.Time) (bool, error) {
	if db == nil || leadID == uuid.Nil {
		return false, nil
	}
	n, err := db.Lead.Update().
		Where(lead.ID(leadID), lead.TenantID(tenantID), lead.StatusEQ(StatusNew)).
		SetStatus("contacted").
		SetContactedAt(at.UTC()).
		Save(ctx)
	return n > 0, err
}
