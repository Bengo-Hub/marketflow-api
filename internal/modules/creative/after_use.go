package creative

import (
	"context"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AfterUseTTL is how long an approved video is kept once it has been used (owner, 2026-11-10).
const AfterUseTTL = 48 * time.Hour

// afterUseDue reports whether an approved video has been used and nothing still needs it: every
// publish job it has is finished and at least one was posted, or the client downloaded it; and no
// boost is still waiting to launch with it. A video whose posts all failed or were cancelled is
// kept, since the client may retry.
func afterUseDue(jobStatuses []string, downloaded, boostBusy bool) bool {
	if boostBusy {
		return false
	}
	posted := false
	for _, s := range jobStatuses {
		switch s {
		case "published":
			posted = true
		case "failed", "cancelled":
		default:
			return false // pending, uploading or processing
		}
	}
	return posted || downloaded
}

// AfterUseNotifier tells the client when a used video will be removed, with its link.
type AfterUseNotifier func(ctx context.Context, tenantID, assetID uuid.UUID, url string, until time.Time)

// EmailAfterUse emails the tenant's admins the video link and when it will be removed, so they
// can download a copy. One email per video and address (the idempotency key is the asset id).
func EmailAfterUse(db *ent.Client, notif *notifications.Client, appURL string, logger *zap.Logger) AfterUseNotifier {
	return func(ctx context.Context, tenantID, assetID uuid.UUID, url string, until time.Time) {
		if notif == nil {
			return
		}
		emails, err := rbac.TenantAdminEmails(ctx, db, tenantID, rbac.NoticeRoles)
		if err != nil || len(emails) == 0 {
			logger.Warn("no admin to tell about a video's removal", zap.String("tenant", tenantID.String()), zap.Error(err))
			return
		}
		studio := appURL + "/studio"
		if t, err := db.Tenant.Get(ctx, tenantID); err == nil && t.Slug != "" {
			studio = appURL + "/" + t.Slug + "/studio"
		}
		when := until.In(nairobi).Format("Monday 2 January at 15:04")
		body := fmt.Sprintf("Your approved video has been posted or downloaded, so we will remove it from MarketFlow on %s (East Africa Time) to keep storage free.\n\n"+
			"Download a copy now if you want to keep it or post it again later:\n%s\n\nYour studio: %s", when, url, studio)
		for _, email := range emails {
			if err := notif.Send(ctx, tenantID.String(), notifications.Message{
				Channel:        "email",
				Recipient:      email,
				Subject:        "Download your video before " + until.In(nairobi).Format("2 Jan"),
				Body:           body,
				Metadata:       map[string]string{"source": "marketflow", "kind": "video_removal_notice"},
				IdempotencyKey: "video-removal-" + assetID.String() + "-" + email,
			}); err != nil {
				logger.Warn("video removal notice not sent", zap.String("tenant", tenantID.String()), zap.Error(err))
			}
		}
	}
}

var nairobi = func() *time.Location {
	if l, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return l
	}
	return time.FixedZone("EAT", 3*60*60)
}()

// AfterUse starts the 48-hour countdown on an approved video once it has been used, and tells the
// client so they can keep a copy. The purge job then deletes it like any expired draft.
type AfterUse struct {
	DB     *ent.Client
	Notify AfterUseNotifier
	Logger *zap.Logger
	Now    func() time.Time
}

func (u *AfterUse) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

// MarkDownloaded records that the client downloaded the video, then checks it.
func (u *AfterUse) MarkDownloaded(ctx context.Context, tenantID, assetID uuid.UUID) error {
	a, err := u.DB.CreativeAsset.Query().Where(creativeasset.ID(assetID), creativeasset.TenantID(tenantID), creativeasset.PurgedAtIsNil()).Only(ctx)
	if err != nil {
		return err
	}
	md := a.Metadata
	if md == nil {
		md = map[string]any{}
	}
	if _, done := md["downloaded_at"]; !done {
		md["downloaded_at"] = u.now().UTC().Format(time.RFC3339)
		if err := u.DB.CreativeAsset.UpdateOneID(a.ID).SetMetadata(md).Exec(ctx); err != nil {
			return err
		}
	}
	return u.Check(ctx, assetID)
}

// CheckJob checks the video a finished publish job posted.
func (u *AfterUse) CheckJob(ctx context.Context, jobID string) {
	id, err := uuid.Parse(jobID)
	if err != nil {
		return
	}
	j, err := u.DB.PublishJob.Get(ctx, id)
	if err != nil {
		return
	}
	if err := u.Check(ctx, j.AssetID); err != nil && u.Logger != nil {
		u.Logger.Warn("after-use check failed", zap.String("asset", j.AssetID.String()), zap.Error(err))
	}
}

// Check starts the countdown on one approved, unexpired video if it is due. Idempotent: the update
// only applies while expires_at is still empty, and only that writer notifies.
func (u *AfterUse) Check(ctx context.Context, assetID uuid.UUID) error {
	a, err := u.DB.CreativeAsset.Query().
		Where(creativeasset.ID(assetID), creativeasset.KindEQ("video"), creativeasset.ExpiresAtIsNil(), creativeasset.PurgedAtIsNil()).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil // not a kept video: a draft already counting down, purged, or an image
	}
	if err != nil {
		return err
	}
	approved, err := u.DB.CreativeVersion.Query().Where(creativeversion.ID(a.VersionID), creativeversion.StatusEQ(StatusApproved)).Exist(ctx)
	if err != nil || !approved {
		return err
	}
	statuses, err := u.DB.PublishJob.Query().Where(publishjob.TenantID(a.TenantID), publishjob.AssetID(a.ID)).
		Select(publishjob.FieldStatus).Strings(ctx)
	if err != nil {
		return err
	}
	busy, err := u.DB.Campaign.Query().
		Where(campaign.TenantID(a.TenantID), campaign.CreativeVersionID(a.VersionID), campaign.StatusIn("pending", "launching")).Exist(ctx)
	if err != nil {
		return err
	}
	_, downloaded := a.Metadata["downloaded_at"]
	if !afterUseDue(statuses, downloaded, busy) {
		return nil
	}
	until := u.now().UTC().Add(AfterUseTTL)
	n, err := u.DB.CreativeAsset.Update().
		Where(creativeasset.ID(a.ID), creativeasset.ExpiresAtIsNil(), creativeasset.PurgedAtIsNil()).
		SetExpiresAt(until).Save(ctx)
	if err != nil || n == 0 {
		return err
	}
	if u.Notify != nil {
		u.Notify(ctx, a.TenantID, a.ID, a.URL, until)
	}
	return nil
}
