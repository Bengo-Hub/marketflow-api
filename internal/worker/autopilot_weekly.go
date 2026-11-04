package worker

import (
	"context"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
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

// summary reads the agent's briefs of the last 7 days, their versions, the boosts of those
// versions and the boosts' daily metrics. Every query is scoped to the tenant.
func (w *AutopilotWeekly) summary(ctx context.Context, a *ent.CustomAgent) (WeekSummary, error) {
	var out WeekSummary
	since := w.now().UTC().AddDate(0, 0, -7)
	briefs, err := w.db.CreativeBrief.Query().
		Where(creativebrief.TenantID(a.TenantID), func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(s.C(creativebrief.FieldMetadata) + "->>'autopilot_agent_id' = ").Arg(a.ID.String())
			}))
		}).IDs(ctx)
	if err != nil || len(briefs) == 0 {
		return out, err
	}
	versions, err := w.db.CreativeVersion.Query().
		Where(creativeversion.BriefIDIn(briefs...), creativeversion.CreatedAtGTE(since)).All(ctx)
	if err != nil {
		return out, err
	}
	out.AdsMade = len(versions)
	for _, v := range versions {
		out.Credits += v.Credits
	}
	allVersions, err := w.db.CreativeVersion.Query().Where(creativeversion.BriefIDIn(briefs...)).IDs(ctx)
	if err != nil {
		return out, err
	}
	campaigns, err := w.db.Campaign.Query().
		Where(campaign.TenantIDEQ(a.TenantID), campaign.CreativeVersionIDIn(allVersions...)).IDs(ctx)
	if err != nil || len(campaigns) == 0 {
		return out, err
	}
	metrics, err := w.db.AdMetrics.Query().
		Where(admetrics.TenantID(a.TenantID), admetrics.CampaignIDIn(campaigns...), admetrics.DateGTE(since)).All(ctx)
	if err != nil {
		return out, err
	}
	for _, m := range metrics {
		out.Spend += m.Spend
		out.Leads += m.Leads + m.Conversions
	}
	if out.Leads > 0 {
		out.CPL = out.Spend / float64(out.Leads)
	}
	return out, nil
}

func (w *AutopilotWeekly) send(ctx context.Context, a *ent.CustomAgent, s WeekSummary, week string) {
	if w.notif == nil {
		return
	}
	emails, err := tenantAdminEmails(ctx, w.db, a.TenantID, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	body := fmt.Sprintf("Your agent \"%s\" this week:\n\nAds made: %d\nAI credits used: %d\nAd spend: %.0f\nLeads: %d", a.Name, s.AdsMade, s.Credits, s.Spend, s.Leads)
	if s.Leads > 0 {
		body += fmt.Sprintf("\nCost per lead: %.0f", s.CPL)
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
