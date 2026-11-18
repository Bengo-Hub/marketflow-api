package worker

import (
	"context"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// WhatsApp copies of the worker's owner notices (the weekly summary, the reconnect notice), sent
// the way the daily brief is (daily_brief_whatsapp.go): only to approvers who wrote to the
// assistant from their verified number in the last AssistantNumberTTL, so MarketFlow never writes
// first to a number that never used it. Inside a person's 24-hour window the notice goes as a
// short text with a button; outside it only as its utility template once Meta approved it
// (creative.PlatformWhatsApp.SendTemplate checks), and otherwise not at all. The email always
// goes as well.

// waSender is what the notices need from creative.PlatformWhatsApp.
type waSender interface {
	InWindow(ctx context.Context, to string) bool
	SendCTA(ctx context.Context, to, text, button, link, key string) error
	SendTemplate(ctx context.Context, to, kind string, params []string, button, text, key string) (bool, error)
}

// waNotice is one notice's WhatsApp copy.
type waNotice struct {
	Text   string // the in-window text
	Button string // the in-window button's label
	Link   string // the in-window button's link
	// Kind and Params are the template for numbers whose window is closed. Nil Params sends no
	// template (a notice that cannot fill its template's parameters).
	Kind      string
	Params    []string
	URLSuffix string // the template's URL button suffix
	// KeyPrefix makes each person's send go once; the user id is added.
	KeyPrefix string
}

// waNotifier sends notices to a tenant's assistant numbers.
type waNotifier struct {
	wa         waSender
	recipients func(ctx context.Context, tenantID uuid.UUID) []briefRecipient
	logger     *zap.Logger
}

// newWANotifier sends to the approvers among the tenant's remembered assistant numbers.
func newWANotifier(db *ent.Client, rdb *redis.Client, wa waSender, roles []string, logger *zap.Logger) *waNotifier {
	return &waNotifier{wa: wa, logger: logger,
		recipients: func(ctx context.Context, tenantID uuid.UUID) []briefRecipient {
			return assistantRecipients(ctx, db, rdb, tenantID, roles)
		}}
}

// send sends n to each recipient and reports how many went.
func (w *waNotifier) send(ctx context.Context, tenantID uuid.UUID, n waNotice) int {
	if w == nil || w.wa == nil || w.recipients == nil {
		return 0
	}
	sent := 0
	for _, r := range w.recipients(ctx, tenantID) {
		key := n.KeyPrefix + "-" + r.UserID
		var err error
		ok := false
		switch {
		case w.wa.InWindow(ctx, r.Number):
			err = w.wa.SendCTA(ctx, r.Number, n.Text, n.Button, n.Link, key)
			ok = err == nil
		case n.Params != nil:
			ok, err = w.wa.SendTemplate(ctx, r.Number, n.Kind, n.Params, n.URLSuffix, n.Text, key)
		}
		if err != nil {
			if w.logger != nil {
				w.logger.Warn("whatsapp notice not sent", zap.String("tenant", tenantID.String()), zap.String("kind", n.Kind), zap.Error(err))
			}
			continue
		}
		if ok {
			sent++
		}
	}
	return sent
}
