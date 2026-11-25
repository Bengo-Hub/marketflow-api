package creative

import (
	"context"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
)

// Brand faces in video ads (plan item 66 f). The consent records are brandimages/faces.go; here a
// presenter video may use one live face as the model for its actor. The face is checked again at
// every step it could be used: when the storyboard is asked for, when its result arrives, when the
// render starts, when the video is approved. An actor made from a face carries its face_id.

// metaFaceID is the live face the owner chose for a version's presenter.
const metaFaceID = "face_id"

// ErrFaceEnded means the person behind the video's presenter no longer agrees, or the consent ran out.
var ErrFaceEnded = fmt.Errorf("%w: the consent of the person this presenter looks like has ended; make the storyboard again with another presenter", ErrConflict)

// faceRef is a live face sent to marketflow-ai, with a short-lived link to its picture.
type faceRef struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// liveFace is one of the tenant's live faces, read now.
func liveFace(ctx context.Context, db *ent.Client, tenantID uuid.UUID, id string, now func() time.Time) (brandimages.LiveFace, bool) {
	if id == "" {
		return brandimages.LiveFace{}, false
	}
	t, err := db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return brandimages.LiveFace{}, false
	}
	return brandimages.LiveFaceByID(t.Metadata, tenantID, brandimages.FaceRulesFor(ctx, db, tenantID), now(), id)
}

// actorFaceID is the face a version's storyboard actor was made from ("" for a synthetic one).
func actorFaceID(v *ent.CreativeVersion) string {
	return brandimages.VersionFace(v.Metadata)
}

// checkFace refuses a version whose actor was made from a face that is no longer live.
func checkFace(ctx context.Context, db *ent.Client, v *ent.CreativeVersion, now func() time.Time) error {
	id := actorFaceID(v)
	if id == "" {
		return nil
	}
	if _, ok := liveFace(ctx, db, v.TenantID, id, now); !ok {
		return ErrFaceEnded
	}
	return nil
}

// checkFaceChoice refuses a face that is not one of the tenant's live ones, or on a video with no
// presenter.
func (s *Service) checkFaceChoice(ctx context.Context, tenantID uuid.UUID, kind, id string) error {
	if id == "" {
		return nil
	}
	if !isPresenter(kind) {
		return invalid("only a presenter video uses a brand face")
	}
	if _, ok := liveFace(ctx, s.db, tenantID, id, s.nowFn); !ok {
		return invalid("that brand face is not available: its consent is missing or has ended")
	}
	return nil
}

// chooseFace applies the version's chosen face to a storyboard call: an actor made from any other
// face, or from one no longer live, is drawn again. Without a live face the presenter is synthetic.
func (r *Renderer) chooseFace(ctx context.Context, v *ent.CreativeVersion, meta map[string]any, call *storyboardCall) {
	if !isPresenter(call.Kind) {
		return
	}
	have := str(call.Actor[metaFaceID])
	if have != "" {
		if _, ok := liveFace(ctx, r.db, v.TenantID, have, r.nowFn); !ok {
			call.Actor, have = nil, ""
			meta[metaRedrawActor] = true
		}
	}
	want := str(meta[metaFaceID])
	if want == "" {
		return
	}
	f, ok := liveFace(ctx, r.db, v.TenantID, want, r.nowFn)
	if !ok || r.brandLinks == nil {
		return
	}
	links := r.brandLinks.Links(v.TenantID, []string{f.Key})
	if len(links) != 1 {
		return
	}
	if call.Actor != nil && have == f.ID {
		return // the brief's actor is already this person
	}
	call.Face = &faceRef{ID: f.ID, URL: links[0]}
	call.Actor = nil
	meta[metaRedrawActor] = true
}

// usesFace matches rows whose JSON path holds the face id.
func usesFace(field, id string, path ...string) func(*sql.Selector) {
	return func(s *sql.Selector) { s.Where(sqljson.ValueEQ(field, id, sqljson.Path(path...))) }
}

// EndFace purges what was made from a face whose consent ended, for that tenant only: the actor is
// taken off the briefs, and every never-approved version with that actor loses its storyboard and
// its files (stills, the actor, a rendered draft). A file an approved ad, or another video without
// the face, also uses is kept. It is idempotent: the hourly sweep repeats it, which also catches a
// video that was being made at the time. Files that cannot be deleted are left to expire and an
// error is returned.
func (p *Purger) EndFace(ctx context.Context, tenantID uuid.UUID, faceID string) error {
	if faceID == "" {
		return nil
	}
	briefs, err := p.db.CreativeBrief.Query().
		Where(creativebrief.TenantID(tenantID), usesFace(creativebrief.FieldMetadata, faceID, "actor", metaFaceID)).All(ctx)
	if err != nil {
		return err
	}
	for _, b := range briefs {
		meta := copyMap(b.Metadata)
		delete(meta, "actor")
		if err := p.db.CreativeBrief.UpdateOneID(b.ID).SetMetadata(meta).Exec(ctx); err != nil {
			return err
		}
	}
	versions, err := p.db.CreativeVersion.Query().
		Where(creativeversion.TenantID(tenantID), creativeversion.ApprovedAtIsNil(),
			usesFace(creativeversion.FieldMetadata, faceID, "storyboard", "actor", metaFaceID)).All(ctx)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(versions))
	for _, v := range versions {
		ids = append(ids, v.ID)
		if err := p.dropFaceBoard(ctx, v, faceID); err != nil {
			return err
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return p.deleteFaceFiles(ctx, tenantID, ids)
}

// dropFaceBoard takes the storyboard off a version, leaving only which face it was, so the gates
// keep refusing it, and retires a finished draft that shows the face.
func (p *Purger) dropFaceBoard(ctx context.Context, v *ent.CreativeVersion, faceID string) error {
	meta := copyMap(v.Metadata)
	for _, k := range []string{metaGenerationPlan, "scene_cache", metaKeepUnchanged, metaKeepScenes} {
		delete(meta, k)
	}
	meta["storyboard"] = map[string]any{"actor": map[string]any{metaFaceID: faceID}}
	if str(meta[metaFaceID]) == faceID {
		delete(meta, metaFaceID)
	}
	u := p.db.CreativeVersion.Update().Where(creativeversion.ID(v.ID), creativeversion.ApprovedAtIsNil()).SetMetadata(meta)
	if v.Status == StatusReady {
		u.SetStatus(StatusRejected).SetError(ErrFaceEnded.Error())
	}
	return u.Exec(ctx)
}

func (p *Purger) deleteFaceFiles(ctx context.Context, tenantID uuid.UUID, versions []uuid.UUID) error {
	now := p.nowFn()
	assets, err := p.db.CreativeAsset.Query().
		Where(creativeasset.TenantID(tenantID), creativeasset.VersionIDIn(versions...), creativeasset.PurgedAtIsNil()).All(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range assets {
		shared, err := p.db.CreativeAsset.Query().
			Where(creativeasset.TenantID(tenantID), creativeasset.StorageKey(a.StorageKey), creativeasset.PurgedAtIsNil(),
				creativeasset.VersionIDNotIn(versions...)).Exist(ctx)
		if err != nil {
			return err
		}
		if shared {
			// An approved ad (or a video without the face) still uses the file: only this row goes.
			errs = append(errs, p.db.CreativeAsset.UpdateOneID(a.ID).SetPurgedAt(now).Exec(ctx))
			continue
		}
		// The hourly purge deletes what cannot be deleted now.
		if err := p.db.CreativeAsset.UpdateOneID(a.ID).SetExpiresAt(now).Exec(ctx); err != nil {
			return err
		}
		if p.store == nil || !p.store.Enabled() {
			errs = append(errs, ErrFilesUnavailable)
			continue
		}
		if err := p.store.Delete(ctx, a.StorageKey); err != nil {
			errs = append(errs, fmt.Errorf("%w: %v", ErrFilesUnavailable, err))
			continue
		}
		errs = append(errs, p.db.CreativeAsset.UpdateOneID(a.ID).SetPurgedAt(now).Exec(ctx))
	}
	return errors.Join(errs...)
}
