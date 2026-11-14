package publishing

import (
	"context"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
)

// A veto cancels a job still waiting and frees its dedupe key; a job the worker already claimed
// (its lease moved scheduled_at on) or one on the platform is reported as gone.
func TestCancelForVeto(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &EntStore{DB: db}
	planned := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	waiting := seedJob(t, db, StatusPending, planned)
	tid := waiting.TenantID
	sameTenant := func(status string, at time.Time) uuid.UUID {
		asset, account := uuid.New(), uuid.New()
		return db.PublishJob.Create().SetTenantID(tid).SetAssetID(asset).SetConnectedAccountID(account).
			SetPlatform(Facebook).SetStatus(status).SetScheduledAt(at).SetDedupeKey(asset.String() + ":" + account.String()).
			SetMetadata(map[string]any{"planned_at": planned.Format(time.RFC3339)}).SaveX(ctx).ID
	}
	claimed := sameTenant(StatusPending, planned.Add(ClaimLease))
	posted := sameTenant(StatusPublished, planned)

	out, err := s.CancelForVeto(ctx, tid, []uuid.UUID{waiting.ID, claimed, posted})
	if err != nil {
		t.Fatal(err)
	}
	if out.Cancelled != 1 || len(out.Gone) != 2 {
		t.Fatalf("outcome: %+v", out)
	}
	got := db.PublishJob.GetX(ctx, waiting.ID)
	if got.Status != StatusCancelled || got.DedupeKey != "vetoed:"+waiting.ID.String() {
		t.Fatalf("waiting job: %+v", got)
	}
	if db.PublishJob.GetX(ctx, claimed).Status != StatusPending {
		t.Fatal("a claimed job is left to the worker")
	}
	if out, err := s.CancelForVeto(ctx, uuid.New(), []uuid.UUID{claimed}); err != nil || out.Cancelled != 0 || len(out.Gone) != 0 {
		t.Fatalf("another tenant's veto must see nothing: %+v %v", out, err)
	}
	if views, err := s.JobsByID(ctx, tid, []uuid.UUID{waiting.ID, posted}); err != nil || len(views) != 2 {
		t.Fatalf("jobs by id: %v %v", views, err)
	}
}

// Accounts that cannot take the post are skipped with their reason and nothing is scheduled; a
// video of another tenant is refused.
func TestScheduleForVetoSkips(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &EntStore{DB: db}
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("veto " + tid.String()).SetSlug("veto-" + tid.String()).ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.CreativeAsset.Delete().Where(creativeasset.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	b := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("t").SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(1).SetKind("video_ad").SetFormat("1080x1920").SetStatus("ready").SaveX(ctx)
	asset := db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind("video").SetStorageKey("k").SetURL("https://media.example.com/v.mp4").
		SetDurationMs(20000).SetWidth(1080).SetHeight(1920).SaveX(ctx)
	adAccount := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("meta").SetAccountType("meta_ad_account").SetExternalID("act_1").
		SetDisplayName("Ads").SaveX(ctx)
	t.Cleanup(func() { _ = db.ConnectedAccount.DeleteOneID(adAccount.ID).Exec(ctx) })

	out, err := s.ScheduleForVeto(ctx, VetoRequest{TenantID: tid, AssetID: asset.ID, VersionID: v.ID, AgentID: uuid.New(),
		AccountIDs: []string{adAccount.ID.String(), uuid.NewString(), "not-a-uuid"}, Platforms: []string{Facebook}, At: time.Now().Add(4 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Jobs) != 0 || len(out.Skipped) != 2 {
		t.Fatalf("skips: %+v", out)
	}
	if _, err := s.ScheduleForVeto(ctx, VetoRequest{TenantID: uuid.New(), AssetID: asset.ID}); err == nil {
		t.Fatal("another tenant's video must be refused")
	}
}
