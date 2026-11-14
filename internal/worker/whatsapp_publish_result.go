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
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
)

// The result of an Approve and post from WhatsApp (Sprint 17): once every post it scheduled is
// finished (published, failed or cancelled), the requester gets one message: a text with an Open
// calendar button inside their 24-hour window, the mf_publish_result template outside it once
// Meta approved it, otherwise nothing (the calendar and the failure email show it).

// WhatsAppPublishResult sends that message; Check runs from the publishing worker's OnDone.
type WhatsAppPublishResult struct {
	db        *ent.Client
	rdb       *redis.Client
	wa        *creative.PlatformWhatsApp
	portalURL string
	logger    *zap.Logger
}

// NewWhatsAppPublishResult builds the notice.
func NewWhatsAppPublishResult(db *ent.Client, rdb *redis.Client, wa *creative.PlatformWhatsApp, portalURL string, logger *zap.Logger) *WhatsAppPublishResult {
	return &WhatsAppPublishResult{db: db, rdb: rdb, wa: wa, portalURL: strings.TrimSuffix(portalURL, "/"), logger: logger.Named("whatsapp-publish-result")}
}

// publishResultClaim keeps one message per WhatsApp publish.
const publishResultClaim = 7 * 24 * time.Hour

// waRequester reads a job's WhatsApp requester (publishing.MetaWhatsApp).
func waRequester(row *ent.PublishJob) (to, ref string) {
	m, _ := row.Metadata[publishing.MetaWhatsApp].(map[string]any)
	to, _ = m["to"].(string)
	ref, _ = m["ref"].(string)
	return to, ref
}

// publishSummary words the posts of one WhatsApp publish. done is false while any is still going.
func publishSummary(rows []*ent.PublishJob) (summary string, lines []string, done bool) {
	short := make([]string, 0, len(rows))
	for _, r := range rows {
		name := publishing.Title(r.Platform)
		switch r.Status {
		case publishing.StatusPublished:
			short = append(short, name+" posted")
			l := name + ": posted"
			if r.Permalink != "" {
				l += " " + r.Permalink
			}
			lines = append(lines, l)
		case publishing.StatusFailed:
			short = append(short, name+" failed")
			reason := strings.TrimSpace(r.Error)
			if reason == "" {
				reason = "the platform refused it"
			}
			lines = append(lines, name+": not posted ("+clipRunes(reason, 160)+")")
		case publishing.StatusCancelled:
			short = append(short, name+" cancelled")
			lines = append(lines, name+": cancelled")
		default:
			return "", nil, false
		}
	}
	return strings.Join(short, ", "), lines, len(rows) > 0
}

// Check sends the result once the job's whole WhatsApp publish is finished. Errors are logged.
func (n *WhatsAppPublishResult) Check(ctx context.Context, jobID string) {
	if n == nil || n.db == nil || n.wa == nil {
		return
	}
	id, err := uuid.Parse(jobID)
	if err != nil {
		return
	}
	row, err := n.db.PublishJob.Get(ctx, id)
	if err != nil {
		return
	}
	to, ref := waRequester(row)
	if to == "" || ref == "" {
		return
	}
	all, err := n.db.PublishJob.Query().Where(publishjob.TenantIDEQ(row.TenantID), publishjob.AssetIDEQ(row.AssetID)).All(ctx)
	if err != nil {
		n.logger.Warn("publish result: jobs not read", zap.Error(err))
		return
	}
	var mine []*ent.PublishJob
	for _, r := range all {
		if _, rref := waRequester(r); rref == ref {
			mine = append(mine, r)
		}
	}
	summary, lines, done := publishSummary(mine)
	if !done {
		return
	}
	claim := "mf:wa:pubres:" + ref
	if n.rdb != nil {
		ok, err := n.rdb.SetNX(ctx, claim, "1", publishResultClaim).Result()
		if err != nil || !ok {
			return
		}
	}
	business, title, slug := n.names(ctx, row)
	calendar := n.portalURL + "/" + slug + "/publishing"
	text := fmt.Sprintf("Your video ad %s:\n%s", title, strings.Join(lines, "\n"))
	key := "wa-pubres-" + strings.ReplaceAll(ref, ":", "-")
	var sendErr error
	if n.wa.InWindow(ctx, to) {
		sendErr = n.wa.SendCTA(ctx, to, text, "Open calendar", calendar, key)
	} else {
		_, sendErr = n.wa.SendTemplate(ctx, to, creative.TemplatePublishResult, []string{business, title, summary}, slug+"/publishing", text, key)
	}
	if sendErr != nil {
		n.logger.Warn("publish result not sent", zap.String("ref", ref), zap.Error(sendErr))
		if n.rdb != nil {
			n.rdb.Del(context.WithoutCancel(ctx), claim)
		}
	}
}

// names are the business name, the ad's title and the tenant slug for the message.
func (n *WhatsAppPublishResult) names(ctx context.Context, row *ent.PublishJob) (business, title, slug string) {
	business, title = "your business", "your video ad"
	if t, err := n.db.Tenant.Get(ctx, row.TenantID); err == nil {
		business, slug = t.Name, t.Slug
	}
	if a, err := n.db.CreativeAsset.Query().Where(creativeasset.IDEQ(row.AssetID)).Only(ctx); err == nil {
		if v, err := n.db.CreativeVersion.Query().Where(creativeversion.IDEQ(a.VersionID)).Only(ctx); err == nil {
			if b, err := n.db.CreativeBrief.Query().Where(creativebrief.IDEQ(v.BriefID)).Only(ctx); err == nil && b.Title != "" {
				title = b.Title
			}
		}
	}
	return clipRunes(business, 60), clipRunes(title, 60), slug
}
