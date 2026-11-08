package publishing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A waiting post moves and the worker then finds it at the new time. A post the worker has taken
// cannot move, even though it still reads as pending, and neither can another tenant's.
func TestReschedule(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &EntStore{DB: db}
	now := time.Now().UTC()
	at := now.Add(time.Hour).Truncate(time.Second)
	later := now.Add(3 * time.Hour).Truncate(time.Second)

	row := seedJob(t, db, StatusPending, at)
	if _, err := s.Reschedule(ctx, uuid.New(), row.ID, later, now); err == nil {
		t.Fatal("another tenant must not move this post")
	}
	moved, err := s.Reschedule(ctx, row.TenantID, row.ID, later, now)
	if err != nil {
		t.Fatal(err)
	}
	if !moved.ScheduledAt.Equal(later) || !view(moved).PlannedAt.Equal(later) || moved.Metadata["ai_generated"] != true {
		t.Fatalf("moved: %v %v %v", moved.ScheduledAt, view(moved).PlannedAt, moved.Metadata)
	}

	// Claimed: the lease pushed scheduled_at forward and the status is still pending.
	leased := seedJob(t, db, StatusPending, at)
	db.PublishJob.UpdateOneID(leased.ID).SetScheduledAt(at.Add(ClaimLease)).ExecX(ctx)
	if _, err := s.Reschedule(ctx, leased.TenantID, leased.ID, later, now); !errors.Is(err, errNotMovable) {
		t.Fatalf("a claimed post must not move: %v", err)
	}

	// Its time has come, or it is under way.
	due := seedJob(t, db, StatusPending, now.Add(-time.Minute).Truncate(time.Second))
	if _, err := s.Reschedule(ctx, due.TenantID, due.ID, later, now); !errors.Is(err, errNotMovable) {
		t.Fatalf("a due post must not move: %v", err)
	}
	uploading := seedJob(t, db, StatusUploading, at)
	if _, err := s.Reschedule(ctx, uploading.TenantID, uploading.ID, later, now); !errors.Is(err, errNotMovable) {
		t.Fatalf("an uploading post must not move: %v", err)
	}
}
