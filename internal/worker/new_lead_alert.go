package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// leadAlertWindow is the most often a business is emailed about new leads (owner, 2026-11-11):
// the first lead in a window sends the alert; the daily brief keeps the full list.
const leadAlertWindow = 15 * time.Minute

// leadAlertClaim reports whether this tenant may get an alert now, claiming the window if so.
type leadAlertClaim func(ctx context.Context, tenantID uuid.UUID) bool

// redisLeadAlertClaim claims the window with SETNX; without Redis every lead alerts.
func redisLeadAlertClaim(rdb *redis.Client) leadAlertClaim {
	return func(ctx context.Context, tenantID uuid.UUID) bool {
		if rdb == nil {
			return true
		}
		ok, err := rdb.SetNX(ctx, "mf:lead-alert:"+tenantID.String(), 1, leadAlertWindow).Result()
		return err != nil || ok // a Redis error must not silence alerts
	}
}

// NewLeadAlertHandler emails a business's MarketFlow admins and members when a new lead arrives
// (Sprint 18 L58), at most once per leadAlertWindow per business.
type NewLeadAlertHandler struct {
	db     *ent.Client
	notif  *notifications.Client
	appURL string
	claim  leadAlertClaim
	logger *zap.Logger
}

func NewNewLeadAlertHandler(db *ent.Client, rdb *redis.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *NewLeadAlertHandler {
	return &NewLeadAlertHandler{db: db, notif: notif, appURL: strings.TrimRight(appURL, "/"), claim: redisLeadAlertClaim(rdb), logger: logger.Named("lead-alert")}
}

func (h *NewLeadAlertHandler) Subject() string { return "lead.created" }
func (h *NewLeadAlertHandler) Durable() string { return "marketflow-worker-lead-alert" }

func (h *NewLeadAlertHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	ev, err := decodeLeadCreated(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode lead.created", err)
	}
	if h.db == nil || h.notif == nil || !h.claim(ctx, ev.TenantID) {
		return nil
	}
	l, err := h.db.Lead.Get(ctx, ev.LeadID)
	if err != nil {
		h.logger.Warn("lead alert: lead not found", zap.String("lead", ev.LeadID.String()), zap.Error(err))
		return nil
	}
	emails, err := rbac.TenantAdminEmails(ctx, h.db, ev.TenantID, rbac.NoticeRoles)
	if err != nil || len(emails) == 0 {
		return nil
	}
	now := time.Now().In(nairobi)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, nairobi)
	today, _ := h.db.Lead.Query().Where(lead.TenantID(ev.TenantID), lead.CreatedAtGTE(dayStart)).Count(ctx)
	link := h.appURL + "/leads/" + l.ID.String()
	if t, err := h.db.Tenant.Get(ctx, ev.TenantID); err == nil && t.Slug != "" {
		link = h.appURL + "/" + t.Slug + "/leads/" + l.ID.String()
	}
	subject, body := leadAlertText(leadDisplayName(l), l.Source, today, link)
	for _, email := range emails {
		if err := h.notif.Send(ctx, ev.TenantID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: subject, Body: body,
			Metadata:       map[string]string{"source": "marketflow", "kind": "new_lead_alert"},
			IdempotencyKey: "lead-alert-" + l.ID.String() + "-" + email,
		}); err != nil {
			h.logger.Warn("lead alert not sent", zap.String("tenant", ev.TenantID.String()), zap.Error(err))
		}
	}
	return nil
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

// leadAlertText is the alert email's subject and body.
func leadAlertText(name, source string, todayCount int, link string) (string, string) {
	from := map[string]string{
		"meta_ad": "a Meta ad", "tiktok_ad": "a TikTok ad", "funnel": "your funnel page",
		"website": "your website", "widget": "the chat on your site", "whatsapp": "WhatsApp",
	}[source]
	if from == "" {
		from = strings.ReplaceAll(source, "_", " ")
	}
	subject := "New lead: " + name
	body := fmt.Sprintf("%s came in from %s.\n\nOpen the lead: %s\n\n", name, from, link)
	if todayCount > 1 {
		body += fmt.Sprintf("You have %d new leads today. The daily brief lists them all.\n", todayCount)
	}
	body += "\nYou get at most one of these emails every 15 minutes."
	return subject, body
}

// nairobi is East Africa Time, for "today" in the alert.
var nairobi = func() *time.Location {
	if l, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return l
	}
	return time.FixedZone("EAT", 3*3600)
}()
