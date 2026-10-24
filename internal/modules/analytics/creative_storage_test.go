package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
)

// The platform rollup counts stored creative files by state and kind; purged
// files are not counted. Other tests share the table, so it checks deltas.
func TestPlatformUsageCreativeStorage(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	h := NewHandler(db, zap.NewNop())
	tid := uuid.New()
	// Files, then versions, then the brief: the foreign keys need this order.
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.CreativeAsset.Delete().Where(creativeasset.TenantID(tid)).Exec(bg)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantID(tid)).Exec(bg)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(bg)
	})
	figure := func(state, kind string) (int64, int) {
		t.Helper()
		data, err := h.platformUsage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cs := data["creative_storage"].(map[string]any)
		k, _ := cs[state].(map[string]any)["by_kind"].(map[string]any)[kind].(map[string]any)
		if k == nil {
			return 0, 0
		}
		return k["bytes"].(int64), k["files"].(int)
	}
	draftBytes, draftFiles := figure("drafts", "video")
	keptBytes, _ := figure("kept", "video")

	// creative_assets.version_id references creative_versions, so make a real version.
	b := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("storage").SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(1).SetFormat("1080x1350").SaveX(ctx)
	exp := time.Now().Add(time.Hour)
	db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind("video").SetStorageKey("a").SetURL("https://x/a.mp4").
		SetBytes(3_000_000_000).SetExpiresAt(exp).SaveX(ctx) // above int32: SUM must not overflow or fail to scan
	db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind("video").SetStorageKey("b").SetURL("https://x/b.mp4").
		SetBytes(500).SaveX(ctx)
	db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind("video").SetStorageKey("c").SetURL("https://x/c.mp4").
		SetBytes(9999).SetExpiresAt(exp).SetPurgedAt(time.Now()).SaveX(ctx)

	gotDraftBytes, gotDraftFiles := figure("drafts", "video")
	gotKeptBytes, _ := figure("kept", "video")
	if gotDraftBytes-draftBytes != 3_000_000_000 || gotDraftFiles-draftFiles != 1 || gotKeptBytes-keptBytes != 500 {
		t.Fatalf("drafts +%d bytes +%d files, kept +%d bytes", gotDraftBytes-draftBytes, gotDraftFiles-draftFiles, gotKeptBytes-keptBytes)
	}
}
