package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// Sprint 23 L25/L26 (away decision 25): a lead who replies on WhatsApp after the instant
// follow-up is answered by the business's customer assistant, until they ask for a person or
// name a big budget; then the owner takes over and the bot stops.

// leadReplyWindow is how long after the instant follow-up a lead's reply is answered.
const leadReplyWindow = 7 * 24 * time.Hour

// tenantHandoffKey is the tenant metadata amount at or above which a lead goes to the owner.
const tenantHandoffKey = "sales_handoff_value"

// tenantAutoReplyKey switches the assistant's answers to lead replies (unset means on).
const tenantAutoReplyKey = "sales_auto_reply"

// followUpLead finds the lead this sender is, among leads that got the instant follow-up in the
// last leadReplyWindow and were enrolled in a follow-up sequence (the business chose automated
// follow-up). The most recently contacted wins. Leads already handed to the owner are skipped.
func followUpLead(ctx context.Context, db *ent.Client, from string, now time.Time) (*ent.Lead, error) {
	digits := lastNine(from)
	if len(digits) < 9 {
		return nil, nil
	}
	since := now.Add(-leadReplyWindow).UTC().Format(time.RFC3339)
	rows, err := db.Lead.Query().
		Where(lead.PhoneHasSuffix(digits), func(s *entsql.Selector) {
			col := s.C(lead.FieldMetadata)
			// P, not ExprP: ExprP leaves a raw "?" that Postgres rejects.
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(col + "->>'first_reply_at' >= ").Arg(since)
			}))
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(col + "->>'sales_handoff_at' IS NULL")
			}))
		}).
		Order(ent.Desc(lead.FieldUpdatedAt)).Limit(5).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range rows {
		enrolled, err := db.NurtureEnrollment.Query().Where(nurtureenrollment.LeadID(l.ID)).Exist(ctx)
		if err == nil && enrolled {
			return l, nil
		}
	}
	return nil, nil
}

type leadReplyAnswer struct {
	Reply         string `json:"reply"`
	Handoff       bool   `json:"handoff"`
	HandoffReason string `json:"handoff_reason"`
}

// handleLeadReply answers a lead's WhatsApp message. It reports whether the message was a lead's.
func (h *WhatsAppAssistantHandler) handleLeadReply(ctx context.Context, in waInbound) (bool, error) {
	l, err := followUpLead(ctx, h.db, in.From, time.Now())
	if err != nil || l == nil {
		return false, err
	}
	phone := ""
	if l.Phone != nil {
		phone = *l.Phone
	}
	if _, phoneOut := contactOptOuts(ctx, h.db, l.TenantID, "", phone); phoneOut {
		return true, nil // opted out: a lead's message, but never answered by the bot
	}
	t, err := h.db.Tenant.Get(ctx, l.TenantID)
	if err != nil {
		return false, err
	}
	threshold, _ := t.Metadata[tenantHandoffKey].(float64)
	ref := "wa-in-" + in.WaMessageID
	_ = logWhatsAppActivity(ctx, h.db, l, "inbound", in.Text, ref)
	// The business switched the assistant off (Sprint 23 L45): the message stays on the
	// timeline for a person to answer.
	if on, set := t.Metadata[tenantAutoReplyKey].(bool); set && !on {
		return true, nil
	}

	ans, err := h.askLeadReply(ctx, t, l, in.Text, threshold)
	if err != nil {
		return true, err
	}
	if ans.Reply != "" && h.notif != nil {
		if err := h.notif.Send(ctx, l.TenantID.String(), notifications.Message{
			Channel: "whatsapp", Recipient: "+" + strings.TrimPrefix(in.From, "+"), Body: ans.Reply,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "sales_follow_up", "lead_id": l.ID.String()},
			IdempotencyKey: "wa-lead-reply-" + in.WaMessageID,
		}); err != nil {
			return true, err
		}
		_ = logWhatsAppActivity(ctx, h.db, l, "outbound", ans.Reply, "wa-out-"+in.WaMessageID)
	}
	if ans.Handoff {
		h.handOff(ctx, t, l, in.Text, ans.HandoffReason)
	}
	return true, nil
}

func (h *WhatsAppAssistantHandler) askLeadReply(ctx context.Context, t *ent.Tenant, l *ent.Lead, text string, threshold float64) (leadReplyAnswer, error) {
	body, _ := json.Marshal(map[string]any{
		"tenant_id": t.ID.String(), "tenant_slug": t.Slug, "lead_id": l.ID.String(), "text": text, "handoff_value": threshold,
		// The business's compliance rules apply to every reply (Sprint 23 L34).
		"business_type": t.BusinessType,
		// Claims the business never makes (brand context): an answer that makes one goes to the owner.
		"banned_claims": tenantBannedClaims(t.Metadata),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.aiURL+"/api/v1/internal/os/lead-reply", bytes.NewReader(body))
	if err != nil {
		return leadReplyAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", h.aiKey)
	resp, err := h.http.Do(req)
	if err != nil {
		return leadReplyAnswer{}, fmt.Errorf("lead-reply: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return leadReplyAnswer{}, fmt.Errorf("lead-reply: status %d", resp.StatusCode)
	}
	var a leadReplyAnswer
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return leadReplyAnswer{}, fmt.Errorf("lead-reply: %w", err)
	}
	return a, nil
}

// handOff stops the bot for this lead and tells the business at once (Sprint 23 L26).
func (h *WhatsAppAssistantHandler) handOff(ctx context.Context, t *ent.Tenant, l *ent.Lead, lastText, reason string) {
	meta := map[string]any{}
	for k, v := range l.Metadata {
		meta[k] = v
	}
	meta["sales_handoff_at"] = time.Now().UTC().Format(time.RFC3339)
	meta["sales_handoff_reason"] = reason
	if err := h.db.Lead.UpdateOneID(l.ID).SetMetadata(meta).Exec(ctx); err != nil {
		h.logger.Warn("hand-off not saved", zap.String("lead", l.ID.String()), zap.Error(err))
	}
	if h.notif == nil {
		return
	}
	emails, err := tenantAdminEmails(ctx, h.db, l.TenantID, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	name := leadDisplayName(l)
	link := h.portalURL + "/" + t.Slug + "/leads/" + l.ID.String()
	body := fmt.Sprintf("%s needs a person to reply (%s).\n\nTheir last message on WhatsApp:\n\"%s\"\n\n"+
		"The assistant has stopped answering this lead. Reply to them on WhatsApp, and see the whole conversation here: %s",
		name, reason, strings.TrimSpace(lastText), link)
	for _, email := range emails {
		if err := h.notif.Send(ctx, l.TenantID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: "Reply to " + name + " yourself", Body: body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "sales_handoff"},
			IdempotencyKey: "sales-handoff-" + l.ID.String() + "-" + email,
		}); err != nil {
			h.logger.Warn("hand-off email not sent", zap.String("lead", l.ID.String()), zap.Error(err))
		}
	}
}

// logWhatsAppActivity records one WhatsApp message on the lead's timeline.
func logWhatsAppActivity(ctx context.Context, db *ent.Client, l *ent.Lead, direction, text, ref string) error {
	if len(text) > 4000 {
		text = text[:4000]
	}
	return db.Activity.Create().SetTenantID(l.TenantID).SetEntityType("lead").SetEntityID(l.ID).
		SetActivityType("whatsapp").SetDirection(direction).SetBody(text).SetSource("agent").
		SetExternalRef(ref).SetPerformedAt(time.Now().UTC()).Exec(ctx)
}

// tenantBannedClaims reads the brand context's banned claims from tenant metadata.
func tenantBannedClaims(meta map[string]any) []string {
	bc, _ := meta[marketingos.MetadataKey].(map[string]any)
	list, _ := bc["banned_claims"].([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
