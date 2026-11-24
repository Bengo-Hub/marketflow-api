package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
)

// Exit reasons of a lead follow-up that stopped early (owner, 2026-11-24). A status reason is
// "lead_" plus the status the lead moved to (lead_contacted, lead_qualified, lead_converted,
// lead_lost).
const (
	ExitLeadReplied   = "lead_replied"
	ExitMeetingBooked = "meeting_booked"
	ExitFollowUpOff   = "follow_up_off"
)

// replyTypes are the activity types an inbound reply from the lead is logged as.
var replyTypes = []string{"email", "sms", "whatsapp", "dm", "comment", "call"}

// followUpStopReason is why a lead.created follow-up must stop before its next step, or "" to
// carry on: the tenant switched its default follow-up off, a meeting was booked, the lead wrote or
// called in after the follow-up started, or the lead's status moved past new.
func followUpStopReason(ctx context.Context, db *ent.Client, e *ent.NurtureEnrollment, seq *ent.NurtureSequence) (string, error) {
	if e.LeadID == nil {
		return "", nil
	}
	leadID := *e.LeadID
	if leadresponse.IsDefaultCopy(seq) {
		t, err := db.Tenant.Get(ctx, e.TenantID)
		if err != nil {
			return "", err
		}
		if s, _ := leadresponse.Load(ctx, db, t); !s.FollowUp {
			return ExitFollowUpOff, nil
		}
	}
	booked, err := db.ScheduledMeeting.Query().
		Where(scheduledmeeting.LeadID(leadID), scheduledmeeting.StatusNEQ("cancelled")).
		Exist(ctx)
	if err != nil {
		return "", err
	}
	if booked {
		return ExitMeetingBooked, nil
	}
	replied, err := db.Activity.Query().
		Where(
			activity.TenantID(e.TenantID),
			activity.EntityType("lead"),
			activity.EntityID(leadID),
			activity.DirectionEQ("inbound"),
			activity.ActivityTypeIn(replyTypes...),
			activity.PerformedAtGT(e.CreatedAt),
		).
		Exist(ctx)
	if err != nil {
		return "", err
	}
	if replied {
		return ExitLeadReplied, nil
	}
	l, err := db.Lead.Get(ctx, leadID)
	if err != nil {
		if ent.IsNotFound(err) {
			return "lead_deleted", nil
		}
		return "", err
	}
	if l.Status != leadresponse.StatusNew {
		return "lead_" + l.Status, nil
	}
	return "", nil
}

// stepWhatsAppTemplate is the Meta template a WhatsApp step names (wa_template), with its body
// parameters read from the step's wa_params variable names and its button suffix rendered from
// wa_button, or nil when the step names none.
func stepWhatsAppTemplate(step map[string]any, vars map[string]any) *notifications.WhatsAppTemplate {
	name := strings.TrimSpace(asString(step, "wa_template"))
	if name == "" {
		return nil
	}
	tpl := &notifications.WhatsAppTemplate{Name: name, ButtonParam: renderTemplate(asString(step, "wa_button"), vars)}
	if list, ok := step["wa_params"].([]any); ok {
		for _, p := range list {
			key, _ := p.(string)
			v, _ := lookupPath(vars, key)
			tpl.Params = append(tpl.Params, fmt.Sprint(nonNil(v)))
		}
	}
	return tpl
}

func nonNil(v any) any {
	if v == nil {
		return ""
	}
	return v
}
