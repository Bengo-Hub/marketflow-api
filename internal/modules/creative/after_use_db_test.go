package creative

import (
	"context"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// An approved video starts its 48-hour countdown once its posts are done, the client is told
// once, a scheduled post or a download decides as the rules say, and nothing else is touched.
func TestAfterUseStartsTheCountdownOnce(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	v, _ := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "a"})
	db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusApproved).ExecX(ctx)

	video := func(name string) *ent.CreativeAsset {
		return db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind("video").
			SetStorageKey("k/" + name + ".mp4").SetURL("https://media.example.com/" + name + ".mp4").SaveX(ctx)
	}
	job := func(a *ent.CreativeAsset, status string) *ent.PublishJob {
		return db.PublishJob.Create().SetTenantID(tid).SetAssetID(a.ID).SetConnectedAccountID(uuid.New()).
			SetPlatform("facebook").SetScheduledAt(time.Now()).SetStatus(status).SetDedupeKey(uuid.NewString()).SaveX(ctx)
	}

	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	var told []uuid.UUID
	u := &AfterUse{DB: db, Now: func() time.Time { return now }, Notify: func(_ context.Context, _, asset uuid.UUID, _ string, until time.Time) {
		if !until.Equal(now.Add(48 * time.Hour)) {
			t.Errorf("until = %v", until)
		}
		told = append(told, asset)
	}}

	posted, waiting, downloaded := video("posted"), video("waiting"), video("downloaded")
	j := job(posted, "published")
	job(waiting, "published")
	job(waiting, "pending")

	u.CheckJob(ctx, j.ID.String())
	u.CheckJob(ctx, j.ID.String()) // a second finished job or a redelivery must not tell twice
	if err := u.Check(ctx, waiting.ID); err != nil {
		t.Fatal(err)
	}
	if err := u.MarkDownloaded(ctx, tid, downloaded.ID); err != nil {
		t.Fatal(err)
	}
	if len(told) != 2 || told[0] != posted.ID || told[1] != downloaded.ID {
		t.Fatalf("told %v", told)
	}
	if got := db.CreativeAsset.GetX(ctx, posted.ID); got.ExpiresAt == nil || !got.ExpiresAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("posted expires %v", got.ExpiresAt)
	}
	if got := db.CreativeAsset.GetX(ctx, waiting.ID); got.ExpiresAt != nil {
		t.Fatal("a video with a post still scheduled must be kept")
	}
	if got := db.CreativeAsset.GetX(ctx, downloaded.ID); got.Metadata["downloaded_at"] == nil || got.ExpiresAt == nil {
		t.Fatalf("downloaded %+v", got)
	}
	if err := u.MarkDownloaded(ctx, uuid.New(), waiting.ID); !ent.IsNotFound(err) {
		t.Fatalf("another tenant's asset: %v", err)
	}
}
