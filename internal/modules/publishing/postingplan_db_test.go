package publishing

import (
	"context"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/modules/publishing/postingplan"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
)

// Booked counts waiting and published posts in one query and leaves out cancelled and failed
// ones; Load layers the tenant's own cadence over the defaults.
func TestPostingPlanBookedAndLoad(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	job := seedJob(t, db, StatusPending, now.Add(time.Hour))
	tid := job.TenantID
	for _, st := range []string{StatusPublished, StatusCancelled, StatusFailed} {
		db.PublishJob.Create().SetTenantID(tid).SetAssetID(job.AssetID).SetConnectedAccountID(job.ConnectedAccountID).
			SetPlatform(YouTube).SetStatus(st).SetScheduledAt(now).SetDedupeKey(st + job.DedupeKey).
			SetMetadata(map[string]any{}).SaveX(ctx)
	}
	booked, err := postingplan.Booked(ctx, db, tid, []string{YouTube, Facebook}, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(booked[YouTube]) != 2 || len(booked[Facebook]) != 0 {
		t.Fatalf("booked = %v", booked)
	}

	if err := metastore.SetKeys(ctx, db, tid, map[string]any{
		postingplan.TenantCadenceKey: map[string]any{"youtube": map[string]any{"count": 1, "per": "week"}},
	}); err != nil {
		t.Fatal(err)
	}
	s := postingplan.Load(ctx, db, tid)
	if s.Timezone != postingplan.DefaultTimezone || s.Platforms[YouTube].Cadence.Count != 1 || s.Platforms[YouTube].CadenceSource != postingplan.SourceTenant {
		t.Fatalf("settings = %+v", s)
	}
}
