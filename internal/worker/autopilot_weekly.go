package worker

import (
	"context"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"go.uber.org/zap"
)

// AutopilotWeekly emails each active ad autopilot's week to the tenant's admins (Sprint 20): ads
// made, credits used, ad spend, leads and cost per lead, all computed from stored rows. One email
// per agent per ISO week (ClaimOnce and the idempotency key).
type AutopilotWeekly struct {
	db     *ent.Client
	notif  *notifications.Client
	appURL string
	logger *zap.Logger
	claim  func(ctx context.Context, key string, ttl time.Duration) bool
	now    func() time.Time
}

func NewAutopilotWeekly(db *ent.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *AutopilotWeekly {
	return &AutopilotWeekly{db: db, notif: notif, appURL: appURL, logger: logger.Named("autopilot-weekly"), claim: sharedcache.ClaimOnce, now: time.Now}
}

// Run checks every 6 hours and sends on Mondays (UTC), so a restart on the day still sends.
func (w *AutopilotWeekly) Run(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		if w.now().UTC().Weekday() == time.Monday {
			w.tick(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *AutopilotWeekly) tick(ctx context.Context) {
	rows, err := w.db.CustomAgent.Query().
		Where(customagent.IsActive(true), func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(s.C(customagent.FieldMetadata) + "->>'kind' = ").Arg(custom_agents.KindAdAutopilot)
			}))
		}).Limit(1000).All(ctx)
	if err != nil {
		w.logger.Warn("load autopilots", zap.Error(err))
		return
	}
	year, week := w.now().UTC().ISOWeek()
	for _, a := range rows {
		if !w.claim(ctx, fmt.Sprintf("marketflow:autopilot-weekly:%s:%d-%02d", a.ID, year, week), 8*24*time.Hour) {
			continue
		}
		s, err := w.summary(ctx, a)
		if err != nil {
			w.logger.Warn("autopilot summary", zap.String("agent", a.ID.String()), zap.Error(err))
			continue
		}
		w.send(ctx, a, s, fmt.Sprintf("%d-%02d", year, week))
	}
}

// WeekSummary is one agent's last seven days.
type WeekSummary struct {
	AdsMade int
	Credits int
	Spend   float64
	Leads   int64
	CPL     float64
}

// summary is the agent's last 7 days, computed by custom_agents.AutopilotWeekSummary, the same
// numbers the portal's autopilot dashboard shows.
func (w *AutopilotWeekly) summary(ctx context.Context, a *ent.CustomAgent) (WeekSummary, error) {
	s, err := custom_agents.AutopilotWeekSummary(ctx, w.db, a.TenantID, a.ID, w.now())
	return WeekSummary{AdsMade: s.AdsMade, Credits: s.Credits, Spend: s.Spend, Leads: s.Leads, CPL: s.CPL}, err
}

func (w *AutopilotWeekly) send(ctx context.Context, a *ent.CustomAgent, s WeekSummary, week string) {
	if w.notif == nil {
		return
	}
	emails, err := tenantAdminEmails(ctx, w.db, a.TenantID, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	// Blank lines between figures: the email renders paragraphs and folds a single line break
	// into a space, which ran every figure into one line.
	body := fmt.Sprintf("Your agent \"%s\" this week:\n\nAds made: %d\n\nAI credits used: %d\n\nAd spend: %.0f\n\nLeads: %d", a.Name, s.AdsMade, s.Credits, s.Spend, s.Leads)
	if s.Leads > 0 {
		body += fmt.Sprintf("\n\nCost per lead: %.0f", s.CPL)
	}
	body += "\n\nSee the details in MarketFlow: " + w.appURL
	for _, email := range emails {
		if err := w.notif.Send(ctx, a.TenantID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: "Your ad agent's week", Body: body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "autopilot_weekly"},
			IdempotencyKey: "autopilot-weekly-" + a.ID.String() + "-" + week + "-" + email,
		}); err != nil {
			w.logger.Warn("weekly summary not sent", zap.String("agent", a.ID.String()), zap.Error(err))
		}
	}
}
