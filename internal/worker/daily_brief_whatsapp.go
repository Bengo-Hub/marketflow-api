package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
)

// The daily brief on WhatsApp (Sprint 17). The email brief always goes. On WhatsApp it goes only
// to owners and MarketFlow admins who wrote to the assistant from their verified number in the
// last AssistantNumberTTL (so MarketFlow never writes first to a number that never used it):
// inside their 24-hour window as a short text with an Open brief button, outside it as the
// mf_daily_brief template once Meta approved it, otherwise not at all.

// AssistantNumberTTL is how long an assistant user's number stays on the brief list after their
// last message.
const AssistantNumberTTL = 30 * 24 * time.Hour

// DailyBriefWhatsAppKey in Tenant.metadata switches the WhatsApp brief off when false (the email
// brief has its own switch, DailyBriefOptOutKey).
const DailyBriefWhatsAppKey = "daily_brief_whatsapp"

func assistantNumberKey(tenantID uuid.UUID, userID string) string {
	return creative.AssistantNumberKey(tenantID, userID)
}

func assistantUsersKey(tenantID uuid.UUID) string { return "mf:wa:brief-users:" + tenantID.String() }

// RememberAssistantNumber records that the user wrote to the assistant for the tenant from number.
func RememberAssistantNumber(ctx context.Context, rdb *redis.Client, tenantID uuid.UUID, userID, number string) {
	if rdb == nil || userID == "" || number == "" {
		return
	}
	rdb.Set(ctx, assistantNumberKey(tenantID, userID), number, AssistantNumberTTL)
	rdb.SAdd(ctx, assistantUsersKey(tenantID), userID)
	rdb.Expire(ctx, assistantUsersKey(tenantID), AssistantNumberTTL)
}

// briefRecipient is one approving user's WhatsApp number.
type briefRecipient struct {
	UserID string
	Number string
}

// assistantRecipients are the tenant's remembered assistant numbers whose users still hold an
// approving role. Expired entries are dropped from the list.
func assistantRecipients(ctx context.Context, db *ent.Client, rdb *redis.Client, tenantID uuid.UUID, roles []string) []briefRecipient {
	if rdb == nil {
		return nil
	}
	users, err := rdb.SMembers(ctx, assistantUsersKey(tenantID)).Result()
	if err != nil {
		return nil
	}
	var out []briefRecipient
	seen := map[string]bool{}
	for _, uid := range users {
		number, err := rdb.Get(ctx, assistantNumberKey(tenantID, uid)).Result()
		if err != nil || number == "" {
			rdb.SRem(ctx, assistantUsersKey(tenantID), uid)
			continue
		}
		id, err := uuid.Parse(uid)
		if err != nil || seen[number] {
			continue
		}
		if db != nil && !rbac.AuthUserHasAnyRole(ctx, db, tenantID, id, roles) {
			continue
		}
		seen[number] = true
		out = append(out, briefRecipient{UserID: uid, Number: number})
	}
	return out
}

// WithWhatsApp turns on the WhatsApp brief: rdb holds the remembered numbers, wa sends from the
// platform number (with WithRich for the window and WithTemplates for the template).
func (j *DailyBrief) WithWhatsApp(rdb *redis.Client, wa *creative.PlatformWhatsApp) *DailyBrief {
	j.rdb, j.wa = rdb, wa
	return j
}

// briefWhatsAppText is the short WhatsApp form: the first highlights, then where the page is.
func briefWhatsAppText(tenantName, date string, w briefWords) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Good morning. Your marketing brief for %s (%s) is ready.", tenantName, date)
	lines := w.Highlights
	if len(lines) == 0 {
		lines = w.Insights
	}
	for i, l := range lines {
		if i == 2 {
			break
		}
		b.WriteString("\n- " + strings.TrimSpace(l))
	}
	b.WriteString("\n\nOpen it for the full page and today's suggested actions.")
	return b.String()
}

// sendWhatsApp sends the brief to the tenant's assistant numbers; it reports how many went.
func (j *DailyBrief) sendWhatsApp(ctx context.Context, t *ent.Tenant, date string, day time.Time, w briefWords) int {
	if j.wa == nil || j.rdb == nil {
		return 0
	}
	if v, ok := t.Metadata[DailyBriefWhatsAppKey].(bool); ok && !v {
		return 0
	}
	link := j.portalURL + "/" + t.Slug + "/dashboard"
	label := day.Format("Monday 2 January")
	text := briefWhatsAppText(t.Name, label, w)
	sent := 0
	for _, r := range assistantRecipients(ctx, j.db, j.rdb, t.ID, briefRoles) {
		key := "os-brief-wa-" + t.ID.String() + "-" + date + "-" + r.UserID
		var err error
		ok := false
		if j.wa.InWindow(ctx, r.Number) {
			err = j.wa.SendCTA(ctx, r.Number, text, "Open brief", link, key)
			ok = err == nil
		} else {
			ok, err = j.wa.SendTemplate(ctx, r.Number, creative.TemplateDailyBrief, []string{t.Name, label}, t.Slug+"/dashboard", text, key)
		}
		if err != nil {
			j.logger.Warn("daily brief not sent on WhatsApp", zap.String("tenant", t.ID.String()), zap.Error(err))
			continue
		}
		if ok {
			sent++
		}
	}
	return sent
}
