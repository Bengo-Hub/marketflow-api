package creative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The storyboard step of a video ad (Sprint 15): before any video is paid for, the owner
// sees one still per scene, about 1 credit in all, and approves it. A presenter ad's one
// actor is drawn here and kept on the brief (metadata.actor), so every version of the brief
// has the same presenter. A presenter render needs the approved storyboard.
//
// The storyboard runs as a media job like a render: the version goes queued then rendering
// with metadata.stage "storyboard", marketflow-ai pushes the result (kind "storyboard"),
// and the version returns to script_ready with metadata.storyboard. Everything is metadata.

// storyboardApproved reports whether the owner approved this version's storyboard.
func storyboardApproved(v *ent.CreativeVersion) bool {
	sb, ok := v.Metadata["storyboard"].(map[string]any)
	return ok && str(sb["approved_at"]) != ""
}

// MakeStoryboard queues the storyboard of a script waiting for review.
func (s *Service) MakeStoryboard(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if v.Kind != KindVideoAd || v.Status != StatusScriptReady {
		return nil, fmt.Errorf("%w: only a video ad script waiting for review gets a storyboard", ErrConflict)
	}
	if _, ok := v.Copy["script"]; !ok {
		return nil, fmt.Errorf("%w: this video ad has no script", ErrConflict)
	}
	meta := copyMap(v.Metadata)
	meta["stage"] = StageStoryboard
	delete(meta, "attempts")
	delete(meta, "job_started_at")
	delete(meta, "storyboard_error")
	n, err := s.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.TenantID(tenantID), creativeversion.StatusEQ(StatusScriptReady)).
		SetStatus(StatusQueued).SetMetadata(meta).ClearError().
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: the script is no longer waiting for review", ErrConflict)
	}
	return s.version(ctx, tenantID, id)
}

// ApproveStoryboard records the owner's approval of the stills.
func (s *Service) ApproveStoryboard(ctx context.Context, tenantID, id uuid.UUID, by *uuid.UUID) (*ent.CreativeVersion, error) {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	sb, ok := v.Metadata["storyboard"].(map[string]any)
	if v.Kind != KindVideoAd || v.Status != StatusScriptReady || !ok || sb["stills"] == nil {
		return nil, fmt.Errorf("%w: there is no storyboard to approve", ErrConflict)
	}
	meta := copyMap(v.Metadata)
	sb = copyMap(sb)
	sb["approved_at"] = s.nowFn().UTC().Format(time.RFC3339)
	if by != nil {
		sb["approved_by"] = by.String()
	}
	meta["storyboard"] = sb
	n, err := s.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.TenantID(tenantID), creativeversion.StatusEQ(StatusScriptReady)).
		SetMetadata(meta).Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: the script is no longer waiting for review", ErrConflict)
	}
	return s.version(ctx, tenantID, id)
}

// ReviseVideoAd starts a new script of a finished video ad ("change this scene"): the same
// script, end card, voice and approved storyboard, waiting for the owner's edits. Its
// render reuses every scene that did not change (the scene cache).
func (s *Service) ReviseVideoAd(ctx context.Context, tenantID, sourceID uuid.UUID) (*ent.CreativeVersion, error) {
	src, err := s.version(ctx, tenantID, sourceID)
	if err != nil {
		return nil, err
	}
	if src.Kind != KindVideoAd || (src.Status != StatusReady && src.Status != StatusApproved) {
		return nil, fmt.Errorf("%w: only a finished video ad can be changed scene by scene", ErrConflict)
	}
	if err := s.checkLiveDrafts(ctx, tenantID); err != nil {
		return nil, err
	}
	meta := map[string]any{}
	for _, k := range []string{"seconds", "language", "photo_urls", "footage_urls", "video_kind", "voice", "delivery",
		"voice_gender", "voice_accent", "actor_prompt", "storyboard", "scene_cache", "findings", "restricted_categories",
		"music_url", "music_licensed_confirmed", metaBrandImageKeys} {
		if val, ok := src.Metadata[k]; ok {
			meta[k] = val
		}
	}
	// A voice picked by the first render stays, so changed scenes sound like the rest.
	if str(meta["voice"]) == "" && str(src.Metadata["voice_used"]) != "" {
		meta["voice"] = src.Metadata["voice_used"]
	}
	meta["stage"], meta["revision_of"] = StageScript, src.ID.String()
	v, err := s.insertVersion(ctx, tenantID, src.BriefID, &src.ID, KindVideoAd, src.Tier, src.Format, copyMap(src.Copy), meta)
	if err != nil {
		return nil, err
	}
	if err := s.db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusScriptReady).Exec(ctx); err != nil {
		return nil, err
	}
	return s.version(ctx, tenantID, v.ID)
}

// storyboardCall is marketflow-ai's POST /api/v1/internal/creative/storyboard body.
type storyboardCall struct {
	TenantID    string         `json:"tenant_id"`
	VersionID   string         `json:"version_id"`
	Format      string         `json:"format"`
	Kind        string         `json:"kind"`
	Scenes      []ScriptScene  `json:"scenes"`
	PhotoURLs   []string       `json:"photo_urls"`
	Actor       map[string]any `json:"actor,omitempty"`
	ActorPrompt string         `json:"actor_prompt,omitempty"`
	Charge      bool           `json:"charge"`
}

// startStoryboard starts a claimed version's storyboard job in marketflow-ai.
func (r *Renderer) startStoryboard(ctx context.Context, v *ent.CreativeVersion) {
	meta := copyMap(v.Metadata)
	var script VideoScript
	if b, err := json.Marshal(v.Copy["script"]); err != nil || json.Unmarshal(b, &script) != nil || len(script.Scenes) == 0 {
		r.failStoryboard(ctx, v.ID, "this video ad has no script; write it again")
		return
	}
	call := storyboardCall{TenantID: v.TenantID.String(), VersionID: v.ID.String(), Format: v.Format, Kind: videoKind(v),
		Scenes: script.Scenes, PhotoURLs: r.adPhotos(v), ActorPrompt: str(meta["actor_prompt"]), Charge: true}
	if b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx); err == nil {
		if actor, ok := b.Metadata["actor"].(map[string]any); ok && str(actor["url"]) != "" {
			call.Actor = actor
		}
	}
	r.startJob(ctx, v, meta, "/api/v1/internal/creative/storyboard", call)
}

// failStoryboard returns the version to its script with the reason; the script is kept.
func (r *Renderer) failStoryboard(ctx context.Context, id uuid.UUID, msg string) {
	v, err := r.db.CreativeVersion.Get(ctx, id)
	if err != nil {
		return
	}
	meta := copyMap(v.Metadata)
	meta["stage"], meta["storyboard_error"] = StageScript, excerpt(msg, 480)
	_ = r.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.StatusIn(StatusRendering, StatusQueued)).
		SetStatus(StatusScriptReady).SetMetadata(meta).Exec(ctx)
}

// settleStoryboard records a storyboard job's outcome: the stills and the actor on the
// version, the actor on the brief (kept for its later versions), the stills as keyframe
// assets so the purge removes them with the draft.
func (r *Renderer) settleStoryboard(ctx context.Context, v *ent.CreativeVersion, job Job) error {
	switch {
	case job.Status == "done" && job.Result != nil && job.Result.Storyboard != nil:
		sb := job.Result.Storyboard
		meta := copyMap(v.Metadata)
		stills := make([]any, 0, len(sb.Stills))
		for _, st := range sb.Stills {
			stills = append(stills, toMap(st))
		}
		board := map[string]any{"stills": stills, "made_at": r.nowFn().UTC().Format(time.RFC3339), "cost_usd": sb.CostUSD}
		if sb.Actor != nil {
			board["actor"] = toMap(sb.Actor)
		}
		meta["storyboard"], meta["stage"] = board, StageScript
		delete(meta, "storyboard_error")
		settled := false // this report moved the version, so its message goes once
		err := withTx(ctx, r.db, func(tx *ent.Tx) error {
			n, err := tx.CreativeVersion.Update().
				Where(creativeversion.ID(v.ID), creativeversion.StatusEQ(StatusRendering)).
				SetStatus(StatusScriptReady).SetMetadata(meta).SetCredits(v.Credits + job.CreditsCharged).ClearError().
				Save(ctx)
			if err != nil || n == 0 {
				return err
			}
			settled = true
			for _, st := range sb.Stills {
				if st.Key == "" || st.Source != "generated" {
					continue
				}
				if err := r.recordAsset(ctx, tx, v, AssetKeyframe, st.Key, st.URL, map[string]any{"scene_id": st.SceneID}); err != nil {
					return err
				}
			}
			if sb.Actor != nil && sb.Actor.Key != "" {
				return r.recordAsset(ctx, tx, v, AssetKeyframe, sb.Actor.Key, sb.Actor.URL, map[string]any{"actor": true})
			}
			return nil
		})
		if err != nil {
			return err
		}
		if sb.Actor != nil && sb.Actor.URL != "" {
			r.keepActor(ctx, v.BriefID, sb.Actor)
		}
		if settled {
			// A WhatsApp turn's video: the stills go to the requester with the render code.
			r.notifyOSTurn(ctx, v, OSTurnStoryboardReady, stillLinks(sb.Stills), "")
		}
		return nil
	case job.Status == "failed" && job.Refunded:
		msg := job.Error
		if msg == "" {
			msg = "the storyboard could not be made right now; nothing was charged"
		}
		r.failStoryboard(ctx, v.ID, msg)
		return nil
	case job.Status == "failed":
		if intOf(v.Metadata["attempts"]) >= maxAttempts {
			r.refundLost(ctx, v, "storyboard")
			r.failStoryboard(ctx, v.ID, "the storyboard took too long; try again")
			return nil
		}
		return r.db.CreativeVersion.Update().Where(creativeversion.ID(v.ID), creativeversion.StatusEQ(StatusRendering)).
			SetStatus(StatusQueued).Exec(ctx)
	}
	return nil
}

// keepActor stores the actor on the brief, once: an existing actor is never replaced.
func (r *Renderer) keepActor(ctx context.Context, briefID uuid.UUID, actor *storyboardActor) {
	b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(briefID)).Only(ctx)
	if err != nil {
		return
	}
	if old, ok := b.Metadata["actor"].(map[string]any); ok && str(old["url"]) != "" {
		return
	}
	meta := copyMap(b.Metadata)
	meta["actor"] = toMap(actor)
	if err := r.db.CreativeBrief.UpdateOneID(briefID).SetMetadata(meta).Exec(ctx); err != nil {
		r.logger.Warn("actor not kept on the brief", zap.String("brief_id", briefID.String()), zap.Error(err))
	}
}

// recordAsset adds a stored file to a version so the purge deletes it with the draft.
func (r *Renderer) recordAsset(ctx context.Context, tx *ent.Tx, v *ent.CreativeVersion, kind, key, url string, meta map[string]any) error {
	_, err := tx.CreativeAsset.Create().
		SetTenantID(v.TenantID).SetVersionID(v.ID).SetKind(kind).
		SetStorageKey(key).SetURL(url).SetBytes(0).SetWidth(0).SetHeight(0).SetDurationMs(0).
		SetExpiresAt(r.nowFn().Add(DraftTTL)).SetMetadata(meta).
		Save(ctx)
	return err
}

// refundLost asks marketflow-ai to return the charge of a job lost with its pod, once
// the version is given up on (out of attempts or past its deadline). marketflow-ai refunds
// only a charged job that is not live, finished or already refunded.
func (r *Renderer) refundLost(ctx context.Context, v *ent.CreativeVersion, kind string) {
	path := "/api/v1/internal/creative/jobs/" + v.ID.String() + "/refund"
	if kind == "storyboard" {
		path += "?kind=storyboard"
	}
	data, status, err := r.aiCall(ctx, http.MethodPost, path, map[string]string{"tenant_id": v.TenantID.String()}, 15*time.Second)
	switch {
	case err != nil:
		r.logger.Error("lost job refund not sent", zap.String("version_id", v.ID.String()), zap.Error(err))
	case status != http.StatusOK && status != http.StatusNotFound:
		r.logger.Error("lost job refund refused", zap.String("version_id", v.ID.String()), zap.Int("status", status), zap.ByteString("body", data))
	}
}

// QuoteVideoAd asks marketflow-ai what rendering this version would cost now, scene by
// scene (the scene cache counted), against the tenant's credits. Nothing is charged.
func (r *Renderer) QuoteVideoAd(ctx context.Context, v *ent.CreativeVersion) (json.RawMessage, int, error) {
	if v.Kind != KindVideoAd {
		return nil, 0, fmt.Errorf("%w: only a video ad has a render quote", ErrConflict)
	}
	var script VideoScript
	if b, err := json.Marshal(v.Copy["script"]); err != nil || json.Unmarshal(b, &script) != nil || len(script.Scenes) == 0 {
		return nil, 0, fmt.Errorf("%w: this video ad has no script", ErrConflict)
	}
	data, status, err := r.aiCall(ctx, http.MethodPost, "/api/v1/internal/creative/ad/quote", r.adCallFor(ctx, v, script, false), 25*time.Second)
	if err != nil {
		return nil, 0, err
	}
	if !json.Valid(data) {
		return nil, 0, errors.New("marketflow-ai sent an unreadable quote")
	}
	return data, status, nil
}
