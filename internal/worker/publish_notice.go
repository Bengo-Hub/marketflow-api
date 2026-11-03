package worker

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PublishFailureNotice emails the tenant's admins when a scheduled post fails for good (Sprint
// 16), so a video meant for a launch day is not silently missing. One email per failure: the
// idempotency key is the job id and its scheduled_at, which a retry from the calendar moves, so
// a retried job that fails again is reported again but a redelivered save is not.
type PublishFailureNotice struct {
	db     *ent.Client
	notif  *notifications.Client
	appURL string
	logger *zap.Logger
}

func NewPublishFailureNotice(db *ent.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *PublishFailureNotice {
	return &PublishFailureNotice{db: db, notif: notif, appURL: appURL, logger: logger.Named("publish-notice")}
}

// Notify is the publishing worker's failure hook.
func (n *PublishFailureNotice) Notify(ctx context.Context, j *publishing.Job) {
	if n.notif == nil {
		return
	}
	tenantID, platform, reason := j.TenantID, publishing.Title(j.Platform), j.Error
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return
	}
	emails, err := tenantAdminEmails(ctx, n.db, tid, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		n.logger.Warn("no admin to tell about a failed post", zap.String("tenant", tenantID), zap.Error(err))
		return
	}
	link := n.appURL + "/studio/calendar"
	if t, err := n.db.Tenant.Get(ctx, tid); err == nil && t.Slug != "" {
		link = n.appURL + "/" + t.Slug + "/studio/calendar"
	}
	body := fmt.Sprintf("A video could not be posted to %s.\n\nReason: %s\n\nYou can try again from the posting calendar: %s", platform, reason, link)
	for _, email := range emails {
		err := n.notif.Send(ctx, tenantID, notifications.Message{
			Channel:        "email",
			Recipient:      email,
			Subject:        "A scheduled post did not go out",
			Body:           body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "publish_failed"},
			IdempotencyKey: fmt.Sprintf("publish-failed-%s-%d-%s", j.ID, j.ScheduledAt.Unix(), email),
		})
		if err != nil {
			n.logger.Warn("failed-post notice not sent", zap.String("tenant", tenantID), zap.Error(err))
		}
	}
}
