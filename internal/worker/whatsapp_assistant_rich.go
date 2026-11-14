package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// Reply buttons, lists and the sign-up button for the WhatsApp assistant (Sprint 17, owner
// 2026-11-14). marketflow-ai writes the buttons (the approval code check stays there); this
// worker sends them inside the 24-hour window the message just opened, and keeps the business
// picker and the sign-up reply, which are decided here.

// maxPickRows is a list message's limit; more businesses than that are answered by number.
const maxPickRows = 10

// tenantPickPrefix marks a business list row: "mf:t:<n>", n counting from 1 as in the text.
const tenantPickPrefix = "mf:t:"

// WithRich turns on buttons, lists and media (cfg: the window and media limits), and the one
// Sign up button for a number no MarketFlow user has verified. signupURL empty uses the portal;
// every is how often one number may get that reply.
func (h *WhatsAppAssistantHandler) WithRich(cfg creative.WhatsAppConfig, signupURL string, every time.Duration) *WhatsAppAssistantHandler {
	h.rich, h.richCfg = true, cfg
	h.signupURL = strings.TrimSpace(signupURL)
	h.signupEvery = every
	return h
}

// window is how long a message opens the sender's window for rich sends.
func (h *WhatsAppAssistantHandler) window() time.Duration {
	if h.richCfg.Window > 0 {
		return h.richCfg.Window
	}
	return creative.DefaultWhatsAppConfig().Window
}

// replyRich sends marketflow-ai's buttons or list when there are any (the window is open: the
// sender just wrote), else the text. The text is also what notifications-api sends when Meta
// refuses the interactive message.
func (h *WhatsAppAssistantHandler) replyRich(ctx context.Context, in waInbound, text string, ia *notifications.WhatsAppInteractive) error {
	if h.notif == nil {
		return nil
	}
	if !h.rich || ia == nil {
		return h.reply(ctx, in, text)
	}
	return h.sender().SendRich(ctx, in.From, text, ia, "wa-assist-"+in.WaMessageID)
}

// tenantPickList is the business picker as a list message: one row per business, numbered as in
// the text. nil with more than a list holds (the typed number still works).
func tenantPickList(tenants []assistantTenant) *notifications.WhatsAppInteractive {
	if len(tenants) < 2 || len(tenants) > maxPickRows {
		return nil
	}
	rows := make([]notifications.WhatsAppListRow, 0, len(tenants))
	for i, t := range tenants {
		rows = append(rows, notifications.WhatsAppListRow{ID: fmt.Sprintf("%s%d", tenantPickPrefix, i+1), Title: clipRunes(t.Name, 24)})
	}
	return &notifications.WhatsAppInteractive{
		Body: "You use MarketFlow for more than one business. Choose the one this is for, then send your message again.",
		List: &notifications.WhatsAppList{Button: "Businesses", Rows: rows},
	}
}

// tenantPickRow reads a tapped business row; ok is false for any other id.
func tenantPickRow(replyID string) (int, bool) {
	if !strings.HasPrefix(replyID, tenantPickPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(replyID, tenantPickPrefix))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func clipRunes(s string, n int) string {
	if r := []rune(strings.TrimSpace(s)); len(r) > n {
		return string(r[:n])
	}
	return strings.TrimSpace(s)
}

// signupText is the one reply a number with no MarketFlow user gets.
const signupText = "MarketFlow makes and posts ads for your business right here on WhatsApp. This number is not linked to a MarketFlow account yet. Sign up, verify this phone number in your profile, then message me again."

// signupReply sends the Sign up button to a number no MarketFlow user has verified, at most once
// per signupEvery, and only when the message asks for marketing work: anyone else chatting with
// the platform number is left to staff, as before.
func (h *WhatsAppAssistantHandler) signupReply(ctx context.Context, in waInbound) error {
	if !h.rich || h.notif == nil || h.rdb == nil || !looksLikeMarketing(in.Text) {
		return nil
	}
	link := h.signupURL
	if link == "" {
		link = h.portalURL
	}
	if link == "" {
		return nil
	}
	every := h.signupEvery
	if every <= 0 {
		every = 30 * 24 * time.Hour
	}
	ok, err := h.rdb.SetNX(ctx, "mf:wa:signup:"+in.From, "1", every).Result()
	if err != nil || !ok {
		return err
	}
	if err := h.sender().SendCTA(ctx, in.From, signupText, "Sign up", link, "wa-signup-"+in.WaMessageID); err != nil {
		h.rdb.Del(context.WithoutCancel(ctx), "mf:wa:signup:"+in.From)
		return err
	}
	return nil
}
