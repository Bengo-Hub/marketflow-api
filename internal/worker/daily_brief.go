package worker

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// briefRoles receive the daily brief.
var briefRoles = []string{"marketflow_admin", "fbo_owner"}

// DailyBriefOptOutKey in Tenant.metadata switches the brief off when false
// (set through PUT /tenant/os-settings).
const DailyBriefOptOutKey = marketingos.DailyBriefKey

// briefSendAfter is the tenant-local time the brief goes out from.
const briefSendAfter = 6*time.Hour + 30*time.Minute

const (
	// briefWordsTimeout sits above marketflow-ai's own cap on the wording call (about 20s), so a
	// slow answer that is still coming is not cut off early.
	briefWordsTimeout = 25 * time.Second
	// briefWordsBackoff is how long the wording call is skipped after it fails.
	briefWordsBackoff = 10 * time.Minute
)

// DailyBrief emails each tenant's admins a one-page morning brief: customers
// first (orders, deals won, leads), then spend and cost per lead, yesterday
// against the 7-day average. Numbers are computed here; marketflow-ai only picks
// the three changes that matter and words them. One brief per tenant per local
// day, fleet-wide (ClaimOnce on the tenant and date).
type DailyBrief struct {
	db         *ent.Client
	svc        *marketingos.Service
	notif      *notifications.Client
	aiURL      string
	serviceKey string
	portalURL  string
	hc         *http.Client
	every      time.Duration
	logger     *zap.Logger
	nowFn      func() time.Time
	claim      func(ctx context.Context, key string, ttl time.Duration) bool

	// aiDownUntil skips the wording call after a failure, so when marketflow-ai is down or slow
	// the remaining tenants get their numbers-only brief at once instead of each waiting out
	// the client timeout.
	aiMu        sync.Mutex
	aiDownUntil time.Time
}

// NewDailyBrief builds the job.
func NewDailyBrief(db *ent.Client, svc *marketingos.Service, notif *notifications.Client, aiURL, serviceKey, portalURL string, logger *zap.Logger) *DailyBrief {
	return &DailyBrief{db: db, svc: svc, notif: notif, aiURL: strings.TrimSuffix(aiURL, "/"), serviceKey: serviceKey,
		portalURL: strings.TrimSuffix(portalURL, "/"), hc: &http.Client{Timeout: briefWordsTimeout},
		every: 15 * time.Minute, logger: logger.Named("daily-brief"), nowFn: time.Now, claim: sharedcache.ClaimOnce}
}

// Run ticks every 15 minutes until ctx ends.
func (j *DailyBrief) Run(ctx context.Context) {
	t := time.NewTicker(j.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sent, err := j.tick(ctx)
			if err != nil {
				j.logger.Warn("daily brief scan failed", zap.Error(err))
			} else if sent > 0 {
				j.logger.Info("daily briefs sent", zap.Int("tenants", sent))
			}
		}
	}
}

func (j *DailyBrief) tick(ctx context.Context) (int, error) {
	if j.notif == nil {
		return 0, nil
	}
	sent := 0
	err := forEachCRMTenant(ctx, j.db, func(tenantID uuid.UUID) {
		ok, err := j.forTenant(ctx, tenantID)
		if err != nil {
			j.logger.Warn("daily brief failed", zap.String("tenant", tenantID.String()), zap.Error(err))
		}
		if ok {
			sent++
		}
	})
	return sent, err
}

// forTenant sends today's brief for one tenant when it is due.
func (j *DailyBrief) forTenant(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	t, err := j.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return false, err
	}
	if v, ok := t.Metadata[DailyBriefOptOutKey].(bool); ok && !v {
		return false, nil
	}
	tz := ""
	if t.Timezone != nil {
		tz = *t.Timezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" {
		loc, _ = time.LoadLocation("Africa/Nairobi")
		if loc == nil {
			loc = time.FixedZone("EAT", 3*3600)
		}
	}
	now := j.nowFn().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if now.Sub(dayStart) < briefSendAfter {
		return false, nil
	}
	date := dayStart.Format("2006-01-02")
	// Claim before any work, so replicas and later ticks skip it; a failure after
	// the claim loses that day's brief rather than sending it twice.
	if !j.claim(ctx, "marketflow:os-brief:"+tenantID.String()+":"+date, 36*time.Hour) {
		return false, nil
	}
	metrics, err := j.svc.BriefMetrics(ctx, tenantID, dayStart)
	if err != nil {
		return false, err
	}
	if !marketingos.HasActivity(metrics) {
		return false, nil
	}
	recipients, err := tenantAdminEmails(ctx, j.db, tenantID, briefRoles)
	if err != nil || len(recipients) == 0 {
		return false, err
	}
	lang := "en"
	if strings.EqualFold(stringOr(t.Metadata["brief_language"], ""), "sw") {
		lang = "sw"
	}
	words := j.words(ctx, lang, metrics)
	subject := fmt.Sprintf("Your marketing brief for %s", dayStart.AddDate(0, 0, -1).Format("Mon 2 Jan"))
	body := renderBrief(t.Name, metrics, words, j.portalURL+"/"+t.Slug+"/dashboard")
	delivered := false
	for _, email := range recipients {
		err := j.notif.Send(ctx, tenantID.String(), notifications.Message{
			Channel:        "email",
			Recipient:      email,
			Subject:        subject,
			Body:           body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "daily_brief"},
			IdempotencyKey: "os-brief-" + tenantID.String() + "-" + date + "-" + email,
		})
		if err != nil {
			j.logger.Warn("daily brief not sent", zap.String("tenant", tenantID.String()), zap.Error(err))
			continue
		}
		delivered = true
	}
	return delivered, nil
}

// briefWords is marketflow-ai's reply.
type briefWords struct {
	Highlights []string `json:"highlights"`
	Insights   []string `json:"insights"`
	Actions    []string `json:"actions"`
}

// words asks marketflow-ai for the highlights and wording. On any failure the
// brief still goes out with the numbers alone.
func (j *DailyBrief) words(ctx context.Context, lang string, metrics []marketingos.BriefMetric) briefWords {
	var out briefWords
	if j.aiURL == "" || j.serviceKey == "" {
		return out
	}
	j.aiMu.Lock()
	skip := j.nowFn().Before(j.aiDownUntil)
	j.aiMu.Unlock()
	if skip {
		return out
	}
	m := make([]any, 0, len(metrics))
	for _, x := range metrics {
		m = append(m, x)
	}
	if err := agentS2SCall(ctx, j.hc, j.aiURL+"/api/v1/internal/os/daily-brief", j.serviceKey,
		map[string]any{"language": lang, "metrics": m}, &out); err != nil {
		j.logger.Warn("daily brief wording unavailable; numbers-only briefs for a while", zap.Error(err),
			zap.Duration("skip_for", briefWordsBackoff))
		j.aiMu.Lock()
		j.aiDownUntil = j.nowFn().Add(briefWordsBackoff)
		j.aiMu.Unlock()
		return briefWords{}
	}
	return out
}

// renderBrief builds the email. Every number comes from metrics; model text is
// escaped and limited to the insight and action lines.
func renderBrief(tenantName string, metrics []marketingos.BriefMetric, w briefWords, link string) string {
	var b strings.Builder
	b.WriteString(`<div style="font-family:Arial,Helvetica,sans-serif;color:#1f2933;max-width:560px">`)
	fmt.Fprintf(&b, `<h2 style="margin:0 0 4px">Good morning, %s</h2>`, html.EscapeString(tenantName))
	b.WriteString(`<p style="margin:0 0 16px;color:#52606d">Yesterday against your 7-day average.</p>`)
	b.WriteString(`<table cellpadding="6" style="border-collapse:collapse;width:100%">`)
	for _, m := range metrics {
		fmt.Fprintf(&b, `<tr style="border-bottom:1px solid #e4e7eb"><td>%s</td><td style="text-align:right"><b>%s</b></td><td style="text-align:right;color:#52606d">%s</td></tr>`,
			html.EscapeString(m.Label), html.EscapeString(fmtMetric(m.Value, m.Unit)), html.EscapeString(changeLabel(m.Value, m.Baseline)))
	}
	b.WriteString(`</table>`)
	if len(w.Insights) > 0 {
		b.WriteString(`<h3 style="margin:20px 0 6px">What changed</h3><ul>`)
		for _, s := range w.Insights {
			fmt.Fprintf(&b, `<li>%s</li>`, html.EscapeString(s))
		}
		b.WriteString(`</ul>`)
	}
	if len(w.Actions) > 0 {
		b.WriteString(`<h3 style="margin:16px 0 6px">Next actions</h3><ol>`)
		for _, s := range w.Actions {
			fmt.Fprintf(&b, `<li>%s</li>`, html.EscapeString(s))
		}
		b.WriteString(`</ol>`)
	}
	fmt.Fprintf(&b, `<p style="margin-top:20px"><a href="%s">Open your dashboard</a></p>`, html.EscapeString(link))
	b.WriteString(`<p style="color:#9aa5b1;font-size:12px">To stop these emails, switch off the daily brief in MarketFlow settings.</p></div>`)
	return b.String()
}

func fmtMetric(v float64, unit string) string {
	s := fmt.Sprintf("%.0f", v)
	if unit == "KES" {
		return "KES " + commas(s)
	}
	return s
}

func changeLabel(v, base float64) string {
	if base == 0 {
		if v == 0 {
			return "no change"
		}
		return "new"
	}
	d := (v - base) / base * 100
	switch {
	case d >= 0.5:
		return fmt.Sprintf("up %.0f%%", d)
	case d <= -0.5:
		return fmt.Sprintf("down %.0f%%", -d)
	}
	return "flat"
}

func commas(s string) string {
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func stringOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}
