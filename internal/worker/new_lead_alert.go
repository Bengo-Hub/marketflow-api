package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
)

// Every new lead reaches the team by email (owner, 2026-11-24; email only, to the tenant's
// admins). The first lead of a burst is emailed at once; the leads after it within the alert window
// (lead_response.alert_window, 15 minutes) wait and go out together in one email listing them all,
// sent by the lead response job when the window ends. No lead is ever left out: each lead carries
// metadata.alert_sent_at once an alert named it.

// alertSentKey is the lead metadata key set when a team alert named the lead.
const alertSentKey = "alert_sent_at"

// alertLookback bounds which unalerted leads a flush picks up.
const alertLookback = 24 * time.Hour

// maxLeadsPerAlert bounds one alert email; a larger burst goes out over the next windows.
const maxLeadsPerAlert = 25

// leadAlertClaim reports whether this tenant may get an alert now, claiming the window if so.
type leadAlertClaim func(ctx context.Context, tenantID uuid.UUID, window time.Duration) bool

// redisLeadAlertClaim claims the window with SETNX; without Redis every lead alerts on its own.
func redisLeadAlertClaim(rdb *redis.Client) leadAlertClaim {
	return func(ctx context.Context, tenantID uuid.UUID, window time.Duration) bool {
		if rdb == nil {
			return true
		}
		ok, err := rdb.SetNX(ctx, "mf:lead-alert:"+tenantID.String(), 1, window).Result()
		return err != nil || ok // a Redis error must not silence alerts
	}
}

// NewLeadAlertHandler emails a business's admins about its new leads (Sprint 18 L58, reworked
// 2026-11-24 so a burst is batched, never dropped).
type NewLeadAlertHandler struct {
	db     *ent.Client
	notif  *notifications.Client
	appURL string
	claim  leadAlertClaim
	logger *zap.Logger
	nowFn  func() time.Time
}

func NewNewLeadAlertHandler(db *ent.Client, rdb *redis.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *NewLeadAlertHandler {
	return &NewLeadAlertHandler{db: db, notif: notif, appURL: strings.TrimRight(appURL, "/"), claim: redisLeadAlertClaim(rdb),
		logger: logger.Named("lead-alert"), nowFn: time.Now}
}

func (h *NewLeadAlertHandler) Subject() string { return "lead.created" }
func (h *NewLeadAlertHandler) Durable() string { return "marketflow-worker-lead-alert" }

func (h *NewLeadAlertHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	ev, err := decodeLeadCreated(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode lead.created", err)
	}
	if h.db == nil || h.notif == nil {
		return nil
	}
	t, err := h.db.Tenant.Get(ctx, ev.TenantID)
	if err != nil {
		h.logger.Warn("lead alert: tenant not found", zap.String("tenant", ev.TenantID.String()), zap.Error(err))
		return nil
	}
	_, err = h.alert(ctx, t, ev.LeadID)
	return err
}

// Flush sends the leads of a tenant still waiting for an alert, once the tenant's window is free.
// The lead response job calls it on every tick.
func (h *NewLeadAlertHandler) Flush(ctx context.Context, t *ent.Tenant) (int, error) {
	if h == nil || h.db == nil || h.notif == nil {
		return 0, nil
	}
	since := h.alertSince(ctx, uuid.Nil)
	if since.IsZero() {
		return 0, nil
	}
	waiting, err := h.db.Lead.Query().Where(lead.TenantID(t.ID), lead.CreatedAtGTE(since), notAlerted()).Exist(ctx)
	if err != nil || !waiting {
		return 0, err
	}
	return h.alert(ctx, t, uuid.Nil)
}

// alert claims the tenant's window and emails every lead not yet alerted, newest arrival
// included. A lead that loses the window stays unalerted until Flush. It returns how many leads
// the email named.
func (h *NewLeadAlertHandler) alert(ctx context.Context, t *ent.Tenant, include uuid.UUID) (int, error) {
	window := leadresponse.Platform(ctx, h.db).AlertWindow
	if !h.claim(ctx, t.ID, window) {
		return 0, nil
	}
	since := h.alertSince(ctx, include)
	q := h.db.Lead.Query().Where(lead.TenantID(t.ID), notAlerted())
	if include != uuid.Nil {
		q = q.Where(lead.Or(lead.ID(include), lead.CreatedAtGTE(since)))
	} else {
		q = q.Where(lead.CreatedAtGTE(since))
	}
	rows, err := q.Order(ent.Asc(lead.FieldCreatedAt), ent.Asc(lead.FieldID)).Limit(maxLeadsPerAlert).All(ctx)
	if err != nil {
		return 0, err
	}
	now := h.nowFn().UTC()
	var claimed []*ent.Lead
	for _, l := range rows {
		ok, err := claimLeadMeta(ctx, h.db, l.ID, alertSentKey, now.Format(time.RFC3339))
		if err != nil {
			h.logger.Warn("lead alert claim", zap.String("lead", l.ID.String()), zap.Error(err))
			continue
		}
		if ok {
			claimed = append(claimed, l)
		}
	}
	if len(claimed) == 0 {
		return 0, nil
	}
	emails, err := rbac.TenantAdminEmails(ctx, h.db, t.ID, rbac.NoticeRoles)
	if err != nil || len(emails) == 0 {
		return 0, err
	}
	items := make([]leadAlertItem, 0, len(claimed))
	for _, l := range claimed {
		items = append(items, alertItem(l, t, h.leadLink(t, l.ID)))
	}
	subject, body := leadAlertText(items, window)
	ids := make([]string, len(claimed))
	for i, l := range claimed {
		ids[i] = l.ID.String()
	}
	sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
	for _, email := range emails {
		if err := h.notif.Send(ctx, t.ID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: subject, Body: body,
			Metadata:       map[string]string{"source": "marketflow", "kind": "new_lead_alert"},
			IdempotencyKey: "lead-alert-" + hex.EncodeToString(sum[:8]) + "-" + email,
		}); err != nil {
			h.logger.Warn("lead alert not sent", zap.String("tenant", t.ID.String()), zap.Error(err))
		}
	}
	return len(claimed), nil
}

// alertSince is the earliest arrival an alert picks up: the last day, never before lead response
// went live. Before the start is recorded only the new lead itself is alerted (zero time with no
// lead to include).
func (h *NewLeadAlertHandler) alertSince(ctx context.Context, include uuid.UUID) time.Time {
	now := h.nowFn().UTC()
	since := now.Add(-alertLookback)
	started, ok := leadresponse.StartedAt(ctx, h.db)
	if !ok {
		if include == uuid.Nil {
			return time.Time{}
		}
		return now.Add(time.Hour) // matches nothing but the included lead
	}
	if started.After(since) {
		since = started
	}
	return since
}

func (h *NewLeadAlertHandler) leadLink(t *ent.Tenant, id uuid.UUID) string {
	if t.Slug != "" {
		return h.appURL + "/" + t.Slug + "/leads/" + id.String()
	}
	return h.appURL + "/leads/" + id.String()
}

// notAlerted keeps leads no alert has named yet.
func notAlerted() func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.Not(entsql.ExprP("jsonb_exists(COALESCE(" + s.C(lead.FieldMetadata) + ", '{}'::jsonb), '" + alertSentKey + "')")))
	}
}

// claimLeadMeta sets metadata[key] on a lead only when the key is absent, in one UPDATE that
// merges (metadata || patch). It reports whether this call set it, so replicas never both act.
func claimLeadMeta(ctx context.Context, db *ent.Client, leadID uuid.UUID, key string, value any) (bool, error) {
	patch, _ := json.Marshal(map[string]any{key: value})
	expr := entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("(COALESCE(" + lead.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(patch))
		b.WriteString("::jsonb)")
	})
	n, err := db.Lead.Update().
		Where(lead.IDEQ(leadID), func(s *entsql.Selector) {
			s.Where(entsql.Not(entsql.ExprP("jsonb_exists(COALESCE(" + s.C(lead.FieldMetadata) + ", '{}'::jsonb), '" + key + "')")))
		}).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(lead.FieldMetadata, expr) }).
		Save(ctx)
	return n > 0, err
}

// leadAlertItem is one lead in an alert email.
type leadAlertItem struct {
	Name, Interest, Message, Phone, WhatsApp, Email, Source, Link string
}

// alertItem reads what the team needs to answer a lead: who, what they want, what they wrote,
// how to reach them and where they came from.
func alertItem(l *ent.Lead, t *ent.Tenant, link string) leadAlertItem {
	it := leadAlertItem{Name: leadDisplayName(l), Source: sourceLabel(l.Source), Link: link}
	it.Interest = strings.TrimSpace(deref(l.Interest))
	if svc, _ := l.Metadata["service"].(string); it.Interest == "" {
		it.Interest = strings.TrimSpace(svc)
	}
	for _, k := range []string{"message", "notes", "booking_notes"} {
		if m, _ := l.Metadata[k].(string); strings.TrimSpace(m) != "" {
			it.Message = clip(strings.Join(strings.Fields(m), " "), 500)
			break
		}
	}
	it.Phone = strings.TrimSpace(deref(l.Phone))
	it.Email = strings.TrimSpace(deref(l.Email))
	country := ""
	if t != nil && t.Country != nil {
		country = *t.Country
	}
	it.WhatsApp = whatsAppChatLink(it.Phone, country)
	return it
}

// whatsAppChatLink is a click-to-chat link for a phone number, or "" when it has too few digits.
// A local Kenyan number (0712...) gets its country code, the platform's home market.
func whatsAppChatLink(phone, country string) string {
	d := onlyDigits(phone)
	if len(d) < 9 {
		return ""
	}
	if strings.HasPrefix(d, "0") && len(d) == 10 && (country == "" || strings.EqualFold(country, "KE") || strings.EqualFold(country, "Kenya")) {
		d = "254" + d[1:]
	}
	return "https://wa.me/" + d
}

// leadDisplayName is the lead's name, or a neutral label before enrichment fills it in.
func leadDisplayName(l *ent.Lead) string {
	var parts []string
	if l.FirstName != nil && *l.FirstName != "" {
		parts = append(parts, *l.FirstName)
	}
	if l.LastName != nil && *l.LastName != "" {
		parts = append(parts, *l.LastName)
	}
	if len(parts) == 0 {
		return "A new lead"
	}
	return strings.Join(parts, " ")
}

// sourceLabel names where a lead came from in plain words.
func sourceLabel(source string) string {
	from := map[string]string{
		"meta_ad": "a Meta ad", "tiktok_ad": "a TikTok ad", "funnel": "your funnel page",
		"website": "your website", "website_contact": "your website contact form", "widget": "the chat on your site",
		"whatsapp": "WhatsApp", "booking": "your booking page",
	}[source]
	if from == "" {
		from = strings.ReplaceAll(source, "_", " ")
	}
	return from
}

// leadAlertText is the alert email's subject and plain-text body (notifications-api renders
// each blank-line paragraph as an email paragraph).
func leadAlertText(items []leadAlertItem, window time.Duration) (string, string) {
	if len(items) == 0 {
		return "", ""
	}
	subject := "New lead: " + items[0].Name
	intro := "A new lead just came in. Reply soon: a quick answer is the best way not to lose them."
	if len(items) > 1 {
		subject = fmt.Sprintf("%d new leads: %s and %d more", len(items), items[0].Name, len(items)-1)
		intro = fmt.Sprintf("%d new leads came in. Here they all are, so none of them waits for a reply.", len(items))
	}
	parts := []string{intro}
	for _, it := range items {
		lines := []string{it.Name}
		if it.Interest != "" {
			lines = append(lines, "Interested in: "+it.Interest)
		}
		if it.Message != "" {
			lines = append(lines, "Message: "+it.Message)
		}
		if it.Phone != "" {
			lines = append(lines, "Phone: "+it.Phone)
		}
		if it.WhatsApp != "" {
			lines = append(lines, "WhatsApp: "+it.WhatsApp)
		}
		if it.Email != "" {
			lines = append(lines, "Email: "+it.Email)
		}
		lines = append(lines, "Came in from: "+it.Source, "Open the lead: "+it.Link)
		parts = append(parts, strings.Join(lines, "\n"))
	}
	parts = append(parts, fmt.Sprintf("Leads that arrive within %s of an alert are sent together in the next one, so none is missed.", humanDuration(window)))
	return subject, strings.Join(parts, "\n\n")
}

// humanDuration writes a duration as "15 minutes", "2 hours" or "1 day".
func humanDuration(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0 && d > 24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	case d >= time.Hour && d%time.Hour == 0:
		return plural(int(d/time.Hour), "hour")
	}
	return plural(int(d/time.Minute), "minute")
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
