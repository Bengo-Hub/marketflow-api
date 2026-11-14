package creative

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A discarded draft (owner, 2026-11-14) keeps its row until the window is over, its files expire
// by then, and it is deleted with its brief only when nobody acted on it; an edited one stays.
func TestDiscardedDraftsAreDeletedUnlessActedOn(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	s.nowFn = func() time.Time { return now }

	draft := func() (briefID, versionID uuid.UUID) {
		b := newBrief(t, s, tid)
		v, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "a"})
		if err != nil {
			t.Fatal(err)
		}
		far := now.Add(30 * 24 * time.Hour)
		db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind("image").
			SetStorageKey("k/" + v.ID.String() + ".jpg").SetURL("https://media.example.com/x.jpg").SetExpiresAt(far).SaveX(ctx)
		return b.ID, v.ID
	}
	window := 72 * time.Hour

	leftB, left := draft()
	keptB, kept := draft()
	for _, id := range []uuid.UUID{left, kept} {
		if ok, err := s.MarkDiscarded(ctx, tid, id, window); err != nil || !ok {
			t.Fatalf("mark %v: %v %v", id, ok, err)
		}
	}
	if ok, _ := s.MarkDiscarded(ctx, uuid.New(), left, window); ok {
		t.Fatal("another tenant marked the draft")
	}
	a := db.CreativeAsset.Query().AllX(ctx)
	for _, x := range a {
		if (x.VersionID == left || x.VersionID == kept) && (x.ExpiresAt == nil || x.ExpiresAt.After(now.Add(window))) {
			t.Fatalf("file expiry not brought forward: %v", x.ExpiresAt)
		}
	}
	// The owner opens and changes one draft after the discard.
	db.CreativeVersion.UpdateOneID(kept).SetUpdatedAt(now.Add(2 * time.Hour)).ExecX(ctx)

	// The window passes and the file purge has removed the files.
	db.CreativeAsset.Update().SetPurgedAt(now).ExecX(ctx)
	p := NewPurger(db, nil, zap.NewNop())
	p.nowFn = func() time.Time { return now.Add(window + time.Hour) }
	n, err := p.PurgeDiscarded(ctx, window)
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
	if _, err := db.CreativeVersion.Get(ctx, left); err == nil {
		t.Fatal("the untouched draft was kept")
	}
	if _, err := db.CreativeBrief.Get(ctx, leftB); err == nil {
		t.Fatal("its empty brief was kept")
	}
	if _, err := db.CreativeVersion.Get(ctx, kept); err != nil {
		t.Fatalf("the edited draft was deleted: %v", err)
	}
	if _, err := db.CreativeBrief.Get(ctx, keptB); err != nil {
		t.Fatalf("the edited draft's brief was deleted: %v", err)
	}
}
