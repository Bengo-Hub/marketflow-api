package creative

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/auditlog"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
)

// A discarded poster comes back to ready with its files kept for the draft time, its ad leaves
// the archive, and the discard and the restore are in the activity log. A version whose files
// were purged, or one never discarded, is refused.
func TestRestoreDiscardedVersion(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	v, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "a"})
	if err != nil {
		t.Fatal(err)
	}
	db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusReady).ExecX(ctx)
	db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind("image").
		SetStorageKey("k/a.jpg").SetURL("https://media.example.com/a.jpg").SetExpiresAt(time.Now().Add(time.Hour)).SaveX(ctx)
	if _, err := s.RestoreVersion(ctx, tid, v.ID); !errors.Is(err, ErrNotDiscarded) {
		t.Fatalf("a live version cannot be restored, got %v", err)
	}
	if _, err := s.Discard(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	if got := db.CreativeBrief.GetX(ctx, b.ID); got.Status != "archived" {
		t.Fatalf("brief %s after its only version was discarded", got.Status)
	}
	got, err := s.RestoreVersion(ctx, tid, v.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got.Status != StatusReady || str(got.Metadata[metaDiscardedAt]) != "" || str(got.Metadata[metaDiscardPurgeAt]) != "" {
		t.Fatalf("restored %s %v", got.Status, got.Metadata)
	}
	a := db.CreativeAsset.Query().Where(creativeasset.VersionID(v.ID)).OnlyX(ctx)
	if a.ExpiresAt == nil || a.ExpiresAt.Before(time.Now().Add(DraftTTL-time.Minute)) {
		t.Fatalf("file expiry not reset: %v", a.ExpiresAt)
	}
	if got := db.CreativeBrief.GetX(ctx, b.ID); got.Status != "active" {
		t.Fatalf("brief %s after the restore", got.Status)
	}
	for _, ev := range []string{AuditVersionDiscarded, AuditVersionRestored} {
		if n := db.AuditLog.Query().Where(auditlog.TenantID(tid), auditlog.Event(ev)).CountX(ctx); n != 1 {
			t.Fatalf("%s logged %d times", ev, n)
		}
	}
	if _, err := s.Discard(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	db.CreativeAsset.Update().Where(creativeasset.VersionID(v.ID)).SetPurgedAt(time.Now()).ExecX(ctx)
	if _, err := s.RestoreVersion(ctx, tid, v.ID); !errors.Is(err, ErrFilesGone) {
		t.Fatalf("purged files cannot be restored, got %v", err)
	}
}
