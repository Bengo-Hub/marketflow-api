package creative

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
)

// A brand face reaches the storyboard only while its consent is live, its actor is refused by the
// render and the approval once it ends, and the purge keeps only what an approved ad uses.
func TestBrandFaceInVideoAd(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	s := NewService(db, zap.NewNop())
	tid, other := newTenant(t, db), newTenant(t, db)
	root := t.TempDir()
	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: root, LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	faceID := uuid.NewString()
	for _, id := range []uuid.UUID{tid, other} {
		db.Tenant.Create().SetID(id).SetName("Face " + id.String()).SetSlug("face-" + id.String()).SaveX(ctx)
		t.Cleanup(func() { _ = db.Tenant.DeleteOneID(id).Exec(ctx) })
	}
	setFace := func(ended string) {
		face := map[string]any{"id": faceID, "image_id": "owner", "name": "Jane", "use": "presenter",
			"until": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339), "recorded_by": "admin@example.test", "recorded_at": "x"}
		if ended != "" {
			face["ended_at"] = ended
		}
		tn := db.Tenant.GetX(ctx, tid)
		meta := copyMap(tn.Metadata)
		meta[brandimages.MetadataKey] = []any{map[string]any{"id": "owner", "key": brandimages.Prefix(tid) + "2026/11/owner.jpg"}}
		meta[brandimages.FacesOnKey], meta[brandimages.FacesKey] = true, []any{face}
		db.Tenant.UpdateOneID(tid).SetMetadata(meta).ExecX(ctx)
	}
	setFace("")
	b := newBrief(t, s, tid)
	if _, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Kind: VideoTemplate, FaceID: faceID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a template took a face: %v", err)
	}
	ob := newBrief(t, s, other)
	if _, err := s.CreateVideoAd(ctx, other, ob.ID, VideoAdInput{Kind: VideoPresenter, FaceID: faceID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another tenant used the face: %v", err)
	}
	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Kind: VideoPresenter, FaceID: faceID})
	if err != nil || str(v.Metadata[metaFaceID]) != faceID {
		t.Fatalf("version %v %v", v, err)
	}

	r := NewRenderer(db, "http://ai.test", "k", zap.NewNop()).WithBrandImages(brandimages.NewLinker(store, time.Hour))
	ask := func(actor map[string]any) (storyboardCall, map[string]any) {
		call, meta := storyboardCall{Kind: VideoPresenter, Actor: actor}, map[string]any{metaFaceID: faceID}
		r.chooseFace(ctx, v, meta, &call)
		return call, meta
	}
	if call, meta := ask(map[string]any{"url": "https://x/synthetic.jpg"}); call.Face == nil || call.Face.ID != faceID || call.Face.URL == "" || call.Actor != nil || meta[metaRedrawActor] != true {
		t.Fatalf("live face %+v", call)
	}
	same := map[string]any{"url": "https://x/a.jpg", metaFaceID: faceID}
	if call, _ := ask(same); call.Face != nil || call.Actor == nil {
		t.Fatalf("drew the same person again %+v", call)
	}
	setFace("2026-11-25T10:00:00Z")
	if call, _ := ask(same); call.Face != nil || call.Actor != nil {
		t.Fatalf("an ended face was used %+v", call)
	}
	setFace("")

	// Two versions show the face: one approved, one draft; they share the actor's file.
	board := map[string]any{"stills": []any{}, "actor": map[string]any{"url": "https://x/a.jpg", metaFaceID: faceID}}
	mk := func(status string, approved bool) *ent.CreativeVersion {
		n, err := s.insertVersion(ctx, tid, b.ID, nil, KindVideoAd, "standard", "1920x1080", map[string]any{}, map[string]any{"video_kind": VideoPresenter, "storyboard": board, "stage": StageRender})
		if err != nil {
			t.Fatal(err)
		}
		u := db.CreativeVersion.UpdateOneID(n.ID).SetStatus(status)
		if approved {
			u.SetApprovedAt(time.Now())
		}
		return u.SaveX(ctx)
	}
	done, draft := mk(StatusApproved, true), mk(StatusReady, false)
	put := func(key string, versions ...*ent.CreativeVersion) string {
		k := "marketflow/" + tid.String() + "/creative-drafts/posters/" + key + ".jpg"
		if err := store.Put(ctx, k, jpegBytes, "image/jpeg"); err != nil {
			t.Fatal(err)
		}
		for _, x := range versions {
			db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(x.ID).SetKind(AssetKeyframe).SetStorageKey(k).SetURL("https://x/" + key).SetExpiresAt(time.Now().Add(time.Hour)).SaveX(ctx)
		}
		return k
	}
	shared, own := put("actor", done, draft), put("draft-actor", draft)
	brief := db.CreativeBrief.GetX(ctx, b.ID)
	bm := copyMap(brief.Metadata)
	bm["actor"] = map[string]any{"url": "https://x/a.jpg", metaFaceID: faceID}
	db.CreativeBrief.UpdateOneID(b.ID).SetMetadata(bm).ExecX(ctx)
	if err := checkFace(ctx, db, draft, time.Now); err != nil {
		t.Fatalf("live face refused: %v", err)
	}

	setFace("2026-11-25T10:00:00Z")
	if _, err := s.Approve(ctx, tid, draft.ID, nil); !errors.Is(err, ErrFaceEnded) {
		t.Fatalf("approved after the consent ended: %v", err)
	}
	p := NewPurger(db, store, zap.NewNop())
	if err := p.EndFace(ctx, other, faceID); err != nil {
		t.Fatal(err)
	}
	onDisk := func(key string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(key)))
		return err == nil
	}
	if !onDisk(own) {
		t.Fatal("another tenant's purge deleted a file")
	}
	for i := 0; i < 2; i++ { // repeated by the sweep
		if err := p.EndFace(ctx, tid, faceID); err != nil {
			t.Fatal(err)
		}
	}
	if onDisk(own) || !onDisk(shared) {
		t.Fatalf("files: draft's %v, approved ad's %v", onDisk(own), onDisk(shared))
	}
	if _, ok := db.CreativeBrief.GetX(ctx, b.ID).Metadata["actor"]; ok {
		t.Fatal("the brief kept the actor")
	}
	got := db.CreativeVersion.GetX(ctx, draft.ID)
	if sb, _ := got.Metadata["storyboard"].(map[string]any); got.Status != StatusRejected || sb["stills"] != nil {
		t.Fatalf("draft %s %v", got.Status, got.Metadata["storyboard"])
	}
	if a := db.CreativeVersion.GetX(ctx, done.ID); a.Status != StatusApproved || str(a.Metadata["storyboard"].(map[string]any)["actor"].(map[string]any)["url"]) == "" {
		t.Fatal("the approved ad changed")
	}
}
