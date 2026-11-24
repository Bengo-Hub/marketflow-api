package publishing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/modules/publishing/postingplan"
	"github.com/google/uuid"
)

// "Move to the best time" lands on the platform's next window at or after the asked time, and a
// past or empty time counts from now, never earlier than now.
func TestBestMoveTime(t *testing.T) {
	plan := postingplan.Load(context.Background(), openTestDB(t), uuid.New()) // defaults, EAT
	now := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)                         // 09:00 EAT, a Tuesday
	asked := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)                      // 16:00 EAT
	got := bestMoveTime(plan, "facebook", asked, now)
	if want := plan.BestTime("facebook", asked).UTC().Truncate(time.Second); !got.Equal(want) || got.Before(asked) {
		t.Fatalf("asked %v: got %v, want %v", asked, got, want)
	}
	past := bestMoveTime(plan, "facebook", now.Add(-time.Hour), now)
	if !past.After(now) {
		t.Fatalf("a past time must move after now: %v", past)
	}
	if bestMoveTime(plan, "tiktok", asked, now).Equal(got) && plan.BestTime("tiktok", asked).Equal(plan.BestTime("facebook", asked)) == false {
		t.Fatal("each platform must follow its own windows")
	}
}

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
