package creative

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
)

type fakeFiles struct {
	deleted []string
	fail    bool
}

func (f *fakeFiles) Delete(_ context.Context, key string) error {
	if f.fail {
		return errors.New("storage down")
	}
	f.deleted = append(f.deleted, key)
	return nil
}

// An admin deletes one draft version and it is gone at once with its files; the ad's last
// version, an approved one, or one whose files cannot be deleted stays.
func TestDeleteVersionNow(t *testing.T) {
	db := openDB(t)
	files := &fakeFiles{}
	s := NewService(db, zap.NewNop()).WithFiles(files)
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	first, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "b"})
	if err != nil {
		t.Fatal(err)
	}
	db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(first.ID).SetKind("image").
		SetStorageKey("k/first.jpg").SetURL("https://media.example.com/first.jpg").SaveX(ctx)

	files.fail = true
	if err := s.DeleteVersion(ctx, tid, first.ID); !errors.Is(err, ErrFilesUnavailable) {
		t.Fatalf("a storage failure must keep everything, got %v", err)
	}
	if _, err := db.CreativeVersion.Get(ctx, first.ID); err != nil {
		t.Fatal("the version went although its file stayed")
	}
	files.fail = false
	if err := s.DeleteVersion(ctx, tid, first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.CreativeVersion.Get(ctx, first.ID); err == nil {
		t.Fatal("the version is still there")
	}
	if n := db.CreativeAsset.Query().Where(creativeasset.VersionID(first.ID)).CountX(ctx); n != 0 || len(files.deleted) != 1 {
		t.Fatalf("assets left %d, files deleted %v", n, files.deleted)
	}
	if err := s.DeleteVersion(ctx, tid, second.ID); !errors.Is(err, ErrLastVersion) {
		t.Fatalf("the last version must stay, got %v", err)
	}
	third, _ := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "c"})
	db.CreativeVersion.UpdateOneID(third.ID).SetApprovedAt(time.Now()).ExecX(ctx)
	if err := s.DeleteVersion(ctx, tid, third.ID); !errors.Is(err, ErrVersionApproved) {
		t.Fatalf("an approved version must stay, got %v", err)
	}
}
