package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/Bengo-Hub/reports"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/analytics"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	tenantmod "github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// WeeklyReport emails every active business its week on Mondays (Sprint 7 #52): new leads against
// the week before, converted leads, deals won, ad spend and the top campaigns, with a link to the
// dashboard where the full report downloads as a PDF. A week with nothing in it sends nothing, and
// the owner can turn the email off (metadata.weekly_report = "off", Settings, Reports). One email
// per tenant per ISO week (ClaimOnce and the idempotency key).
type WeeklyReport struct {
	db     *ent.Client
	notif  *notifications.Client
	appURL string
	logger *zap.Logger
	claim  func(ctx context.Context, key string, ttl time.Duration) bool
	now    func() time.Time
	// wa sends the WhatsApp copy (whatsapp_notice.go); nil sends email only.
	wa *waNotifier
	// creditsUsed reads the AI credits the tenant used in [from, to). mf_weekly_summary needs that
	// number, and no service-to-service read of credit use exists yet (subscriptions-api has the
	// ledger behind a signed-in route only), so it is nil: outside the 24-hour window nothing goes
	// on WhatsApp until a source is wired here. Inside the window the text needs no credits.
	creditsUsed func(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (int, bool)
}

const weeklyReportTenantPage = 200

// WithWhatsApp sends the weekly summary on WhatsApp too, to the approvers who use the assistant.
func (w *WeeklyReport) WithWhatsApp(rdb *redis.Client, wa waSender) *WeeklyReport {
	w.wa = newWANotifier(w.db, rdb, wa, tokenNoticeRoles, w.logger)
	return w
}

// weeklySummaryWhatsApp is the WhatsApp copy of the week: leads and posts, and the template's
// parameters (business, posts, leads, credits) only when the credits are known.
func (w *WeeklyReport) weeklySummaryWhatsApp(ctx context.Context, t *ent.Tenant, d analytics.WeekDigest, weekKey string) waNotice {
	posts, _ := w.db.PublishJob.Query().
		Where(publishjob.TenantID(t.ID), publishjob.StatusEQ("published"),
			publishjob.PublishedAtGTE(d.From), publishjob.PublishedAtLT(d.To)).
		Count(ctx)
	text := fmt.Sprintf("Your marketing week at %s, %s to %s: %d new lead%s (%d the week before), %d converted, and %d post%s went out.\n\nOpen the full summary in MarketFlow.",
		t.Name, d.From.Format("2 Jan"), d.To.AddDate(0, 0, -1).Format("2 Jan"),
		d.Leads, plural(d.Leads), d.LeadsBefore, d.Converted, posts, plural(posts))
	n := waNotice{Text: text, Button: "Open summary", Link: w.appURL + "/" + t.Slug + "/dashboard",
		Kind: creative.TemplateWeeklySummary, URLSuffix: t.Slug + "/dashboard",
		KeyPrefix: "weekly-report-wa-" + t.ID.String() + "-" + weekKey}
	if w.creditsUsed != nil {
		if credits, ok := w.creditsUsed(ctx, t.ID, d.From, d.To); ok {
			n.Params = []string{t.Name, fmt.Sprint(posts), fmt.Sprint(d.Leads), fmt.Sprint(credits)}
		}
	}
	return n
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func NewWeeklyReport(db *ent.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *WeeklyReport {
	return &WeeklyReport{db: db, notif: notif, appURL: strings.TrimRight(appURL, "/"), logger: logger.Named("weekly-report"),
		claim: sharedcache.ClaimOnce, now: time.Now}
}

// Run checks every 6 hours and sends on Mondays (UTC), so a restart on the day still sends.
func (w *WeeklyReport) Run(ctx context.Context) {
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

func (w *WeeklyReport) tick(ctx context.Context) {
	if w.notif == nil {
		return
	}
	now := w.now().UTC()
	year, week := now.ISOWeek()
	weekKey := fmt.Sprintf("%d-%02d", year, week)
	var after uuid.UUID
	for ctx.Err() == nil {
		q := w.db.Tenant.Query().
			Where(tenant.StatusEQ("active"), func(s *entsql.Selector) {
				s.Where(entsql.P(func(b *entsql.Builder) {
					b.WriteString("COALESCE(" + s.C(tenant.FieldMetadata) + "->>'" + tenantmod.WeeklyReportKey + "', '') <> ").Arg("off")
				}))
			}).
			Order(ent.Asc(tenant.FieldID)).Limit(weeklyReportTenantPage)
		if after != uuid.Nil {
			q = q.Where(tenant.IDGT(after))
		}
		rows, err := q.All(ctx)
		if err != nil {
			w.logger.Warn("load tenants", zap.Error(err))
			return
		}
		for _, t := range rows {
			w.one(ctx, t, now, weekKey)
		}
		if len(rows) < weeklyReportTenantPage {
			return
		}
		after = rows[len(rows)-1].ID
	}
}

func (w *WeeklyReport) one(ctx context.Context, t *ent.Tenant, now time.Time, weekKey string) {
	if !tenantmod.WeeklyReportOn(t.Metadata) {
		return
	}
	if !w.claim(ctx, "marketflow:weekly-report:"+t.ID.String()+":"+weekKey, 8*24*time.Hour) {
		return
	}
	d, err := analytics.WeeklyDigest(ctx, w.db, t.ID, now)
	if err != nil {
		w.logger.Warn("weekly digest", zap.String("tenant", t.ID.String()), zap.Error(err))
		return
	}
	if d.Empty() {
		return
	}
	if w.wa != nil {
		w.wa.send(ctx, t.ID, w.weeklySummaryWhatsApp(ctx, t, d, weekKey))
	}
	emails, err := tenantAdminEmails(ctx, w.db, t.ID, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	body := weeklyReportBody(d, w.appURL+"/"+t.Slug+"/dashboard")
	for _, email := range emails {
		if err := w.notif.Send(ctx, t.ID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: "Your week in MarketFlow", Body: body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "weekly_report"},
			IdempotencyKey: "weekly-report-" + t.ID.String() + "-" + weekKey + "-" + email,
		}); err != nil {
			w.logger.Warn("weekly report not sent", zap.String("tenant", t.ID.String()), zap.Error(err))
		}
	}
}

// weeklyReportBody is the email text. Every line is its own paragraph: the notifications client
// folds single line breaks into spaces.
func weeklyReportBody(d analytics.WeekDigest, dashboardURL string) string {
	cur := d.Currency
	lines := []string{
		fmt.Sprintf("Here is your week, %s to %s.", d.From.Format("2 Jan"), d.To.AddDate(0, 0, -1).Format("2 Jan")),
		fmt.Sprintf("New leads: %d (%d the week before)", d.Leads, d.LeadsBefore),
		fmt.Sprintf("Leads converted: %d", d.Converted),
	}
	if d.DealsWon > 0 {
		lines = append(lines, fmt.Sprintf("Deals won: %d, worth %s", d.DealsWon, reports.Money(cur, d.WonValue)))
	} else {
		lines = append(lines, "Deals won: 0")
	}
	if d.Spend > 0 {
		s := "Ad spend: " + reports.Money(cur, d.Spend)
		if d.CostPerLead != nil {
			s += " (" + reports.Money(cur, *d.CostPerLead) + " per lead)"
		}
		lines = append(lines, s)
	}
	if len(d.TopCampaigns) > 0 {
		lines = append(lines, "Top campaigns:")
		for i, c := range d.TopCampaigns {
			s := fmt.Sprintf("%d. %s: %d leads", i+1, c.Name, c.Leads)
			if c.Spend > 0 {
				s += ", " + reports.Money(c.Currency, c.Spend) + " spent"
			}
			if c.WonDeals > 0 {
				s += fmt.Sprintf(", %d won", c.WonDeals)
			}
			lines = append(lines, s)
		}
	}
	lines = append(lines,
		"The return on each campaign is on your dashboard, where the full report downloads as a PDF: "+dashboardURL,
		"You can turn this email off in Settings, Reports.")
	return strings.Join(lines, "\n\n")
}
