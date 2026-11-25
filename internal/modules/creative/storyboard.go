package creative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

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

// metaRedrawActor marks a storyboard asked to draw a new actor (StoryboardOptions.RedrawActor).
const metaRedrawActor = "redraw_actor"

// storyboardApproved reports whether the owner approved this version's storyboard.
func storyboardApproved(v *ent.CreativeVersion) bool {
	sb, ok := v.Metadata["storyboard"].(map[string]any)
	return ok && str(sb["approved_at"]) != ""
}

// StoryboardOptions are the owner's choices for a new storyboard.
type StoryboardOptions struct {
	// RedrawActor draws a new presenter instead of the brief's (2026-11-19 audit, finding 9): the
	// new actor replaces the brief's once the storyboard is made. ActorPrompt, when set,
	// describes the new actor; empty keeps the existing description.
	RedrawActor bool   `json:"redraw_actor"`
	ActorPrompt string `json:"actor_prompt"`
	// KeepUnchanged reuses the stills of scenes that did not change (settings_reset.go), so only
	// the changed scenes are drawn and paid for.
	KeepUnchanged bool `json:"keep_unchanged"`
	// FaceID picks the presenter's look from the tenant's live brand faces (faces.go); nil keeps the
	// video's choice and "" goes back to a presenter not modelled on anyone.
	FaceID *string `json:"face_id"`
}

// MakeStoryboard queues the storyboard of a script waiting for review.
func (s *Service) MakeStoryboard(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	return s.MakeStoryboardWith(ctx, tenantID, id, StoryboardOptions{})
}

// MakeStoryboardWith is MakeStoryboard with the owner's options.
func (s *Service) MakeStoryboardWith(ctx context.Context, tenantID, id uuid.UUID, opt StoryboardOptions) (*ent.CreativeVersion, error) {
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
	// The storyboard's stills are paid work toward a video the plan must include.
	if err := s.CheckVideoFeature(ctx, tenantID, VideoFeature(KindVideoAd, videoKind(v))); err != nil {
		return nil, err
	}
	opt.ActorPrompt = strings.TrimSpace(opt.ActorPrompt)
	if utf8.RuneCountInString(opt.ActorPrompt) > 300 {
		return nil, invalid("actor_prompt is up to 300 characters")
	}
	if opt.RedrawActor && !isPresenter(videoKind(v)) {
		return nil, invalid("only a presenter video has an actor to redraw")
	}
	if opt.FaceID != nil {
		if err := s.checkFaceChoice(ctx, tenantID, videoKind(v), *opt.FaceID); err != nil {
			return nil, err
		}
	}
	meta := copyMap(v.Metadata)
	if opt.FaceID != nil {
		delete(meta, metaFaceID)
		if *opt.FaceID != "" {
			meta[metaFaceID] = *opt.FaceID
		}
	}
	meta["stage"] = StageStoryboard
	delete(meta, "attempts")
	delete(meta, "job_started_at")
	delete(meta, "storyboard_error")
	// A new storyboard is a new plan to approve.
	delete(meta, metaGenerationPlan)
	delete(meta, metaRedrawActor)
	delete(meta, metaKeepUnchanged)
	if opt.KeepUnchanged {
		meta[metaKeepUnchanged] = true
	}
	if opt.RedrawActor {
		meta[metaRedrawActor] = true
		if opt.ActorPrompt != "" {
			meta["actor_prompt"] = opt.ActorPrompt
		}
	}
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
	return s.approveStoryboard(ctx, tenantID, id, func(sb map[string]any) {
		sb["approved_by_kind"] = "person"
		if by != nil {
			sb["approved_by"] = by.String()
		}
	})
}

// ApproveStoryboardByAgent records an autopilot agent's approval of the stills in veto-window
// mode. It is kept apart from a person's approval: approved_by_kind "agent" and approved_by_agent
// carry the agent, approved_by stays empty. In veto-window mode the owner chose to let the agent
// approve, so the agent approves the generation plan as it is too, recorded the same way.
func (s *Service) ApproveStoryboardByAgent(ctx context.Context, tenantID, id, agentID uuid.UUID) (*ent.CreativeVersion, error) {
	mark := func(m map[string]any) {
		m["approved_by_kind"] = "agent"
		m["approved_by_agent"] = agentID.String()
	}
	v, err := s.approveStoryboard(ctx, tenantID, id, mark)
	if err != nil {
		return nil, err
	}
	return s.ApprovePlan(ctx, tenantID, id, PlanFingerprint(v), nil, mark)
}

// ApproveStoryboardAndPlan is a person's approval of the storyboard (when the version has one)
// and of the generation plan named by fingerprint, with the scenes the quote showed.
func (s *Service) ApproveStoryboardAndPlan(ctx context.Context, tenantID, id uuid.UUID, by *uuid.UUID, fingerprint string,
	scenes []any) (*ent.CreativeVersion, error) {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if fingerprint != PlanFingerprint(v) {
		return nil, errPlanChanged
	}
	mark := func(m map[string]any) {
		m["approved_by_kind"] = "person"
		if by != nil {
			m["approved_by"] = by.String()
		}
	}
	if sb, ok := v.Metadata["storyboard"].(map[string]any); ok && sb["stills"] != nil && !storyboardApproved(v) {
		if _, err := s.approveStoryboard(ctx, tenantID, id, mark); err != nil {
			return nil, err
		}
	}
	return s.ApprovePlan(ctx, tenantID, id, fingerprint, scenes, mark)
}

func (s *Service) approveStoryboard(ctx context.Context, tenantID, id uuid.UUID, mark func(map[string]any)) (*ent.CreativeVersion, error) {
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
	mark(sb)
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
	if err := s.CheckVideoFeature(ctx, tenantID, VideoFeature(KindVideoAd, videoKind(src))); err != nil {
		return nil, err
	}
	if err := s.checkLiveDrafts(ctx, tenantID); err != nil {
		return nil, err
	}
	meta := map[string]any{}
	for _, k := range []string{"seconds", "language", "photo_urls", "footage_urls", "video_kind", "voice", "delivery",
		"voice_gender", "voice_accent", "actor_prompt", "storyboard", "scene_cache", "music_cache", "findings", "restricted_categories",
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
	PhotoInfo   []photoInfo    `json:"photo_info,omitempty"`
	Actor       map[string]any `json:"actor,omitempty"`
	ActorPrompt string         `json:"actor_prompt,omitempty"`
	Charge      bool           `json:"charge"`
	// Delivery is the ad's voice delivery: "ugc" draws candid phone-style stills.
	Delivery string `json:"delivery,omitempty"`
	// Brand and Place tell the self-review whose ad it is and where the business is (place.go).
	Brand string `json:"brand,omitempty"`
	Place string `json:"place,omitempty"`
	endCardWords
	// PreviousStills are the stills a keep_unchanged storyboard may reuse (settings_reset.go).
	PreviousStills []storyboardStill `json:"previous_stills,omitempty"`
	// Branding is the in-scene branding rules (creative.branding, logo_plate.go).
	Branding *brandingRules `json:"branding,omitempty"`
	// Screens and Screenshots are the real product screens (creative.screens, screens.go).
	Screens     *screenRules `json:"screens,omitempty"`
	Screenshots []screenshot `json:"screenshots,omitempty"`
	// Face is the person the new actor takes its look from, with a live consent (faces.go).
	Face *faceRef `json:"face,omitempty"`
	// LocalCues are the local cue rules (creative.local_cues, cues.go).
	LocalCues *localCues `json:"local_cues,omitempty"`
}

// endCardWords are the end card's words, contacts, colour and logo: marketflow-ai composes them
// over the end card's still as a preview, so the plan shows the end card that renders.
type endCardWords struct {
	Headline string    `json:"headline,omitempty"`
	Subline  string    `json:"subline,omitempty"`
	Price    string    `json:"price,omitempty"`
	CTA      string    `json:"cta,omitempty"`
	Contacts []contact `json:"contacts,omitempty"`
	Accent   string    `json:"accent,omitempty"`
	LogoURL  string    `json:"logo_url,omitempty"`
	// LogoPlate is how the logo is backed (logo_plate.go).
	LogoPlate logoPlate `json:"logo_plate"`
}

// endCardWordsFor reads a version's end card as the render draws it: its words, the tenant's
// contacts in the configured order, the brand colour, the logo and its plate.
func (r *Renderer) endCardWordsFor(ctx context.Context, v *ent.CreativeVersion, b *ent.CreativeBrief, place placeInfo) endCardWords {
	card, _ := v.Copy["end_card"].(map[string]any)
	w := endCardWords{Headline: str(card["headline"]), Subline: str(card["subline"]), Price: str(card["price"]), CTA: str(card["cta"]),
		Contacts: place.Contacts, LogoURL: brandLogo(ctx, r.db, v.TenantID, b), LogoPlate: logoPlateFor(ctx, r.db, v.TenantID, b)}
	if b != nil {
		if brand, ok := b.Metadata["brand"].(map[string]any); ok {
			w.Accent = str(brand["accent"])
		}
	}
	return w
}

// startStoryboard starts a claimed version's storyboard job in marketflow-ai.
func (r *Renderer) startStoryboard(ctx context.Context, v *ent.CreativeVersion) {
	meta := copyMap(v.Metadata)
	var script VideoScript
	if b, err := json.Marshal(v.Copy["script"]); err != nil || json.Unmarshal(b, &script) != nil || len(script.Scenes) == 0 {
		r.failStoryboard(ctx, v.ID, "this video ad has no script; write it again")
		return
	}
	photos, info := r.adPhotos(ctx, v)
	call := storyboardCall{TenantID: v.TenantID.String(), VersionID: v.ID.String(), Format: v.Format, Kind: videoKind(v),
		Scenes: script.Scenes, PhotoURLs: photos, PhotoInfo: info, Charge: true, Delivery: str(meta["delivery"]),
		// The owner's description wins; the script writer's one-line actor is the fallback.
		ActorPrompt: excerpt(firstNonEmpty(str(meta["actor_prompt"]), script.Actor), 300)}
	call.PreviousStills = previousStills(meta)
	call.Branding = brandingFor(ctx, r.db, v.TenantID)
	call.Screens, call.Screenshots = r.screensFor(ctx, v.TenantID)
	call.LocalCues = localCuesFor(ctx, r.db, v.TenantID)
	if b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx); err == nil {
		place := r.resolvePlace(ctx, v.TenantID, b.Offering, false)
		call.Brand, call.Place = place.Brand, place.Line()
		call.endCardWords = r.endCardWordsFor(ctx, v, b, place)
		if actor, ok := b.Metadata["actor"].(map[string]any); ok && str(actor["url"]) != "" {
			call.Actor = actor
			if redraw, _ := meta[metaRedrawActor].(bool); redraw {
				// A redraw keeps the description unless the owner gave a new one, and draws afresh.
				call.Actor = nil
				if str(meta["actor_prompt"]) == "" && str(actor["prompt"]) != "" {
					call.ActorPrompt = excerpt(str(actor["prompt"]), 300)
				}
			}
		}
	}
	r.chooseFace(ctx, v, meta, &call)
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
	n, err := r.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.StatusIn(StatusRendering, StatusQueued)).
		SetStatus(StatusScriptReady).SetMetadata(meta).Save(ctx)
	if err == nil && n > 0 {
		r.notifyAutopilot(ctx, v, AutopilotStoryboardError)
	}
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
		if len(sb.Review) > 0 {
			board["review"] = sb.Review
		}
		if sb.EndCardPreview != nil && sb.EndCardPreview.URL != "" {
			board["end_card_preview"] = toMap(sb.EndCardPreview)
		}
		meta["storyboard"], meta["stage"] = board, StageScript
		delete(meta, "storyboard_error")
		// New stills are a new plan: the owner approves it again.
		delete(meta, metaGenerationPlan)
		redraw, _ := meta[metaRedrawActor].(bool)
		delete(meta, metaRedrawActor)
		delete(meta, metaKeepUnchanged)
		delete(meta, metaKeepScenes)
		// The storyboard job is over: its start time and attempts must not carry into the render.
		delete(meta, "job_started_at")
		delete(meta, "attempts")
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
				// A reused still was recorded with the storyboard that drew it.
				if st.Key == "" || st.Source != "generated" || st.Reused {
					continue
				}
				if err := r.recordAsset(ctx, tx, v, AssetKeyframe, st.Key, st.URL, map[string]any{"scene_id": st.SceneID}); err != nil {
					return err
				}
				// The still without the placed logo is purged with the draft too.
				if st.PlainKey != "" {
					if err := r.recordAsset(ctx, tx, v, AssetKeyframe, st.PlainKey, st.PlainURL, map[string]any{"scene_id": st.SceneID, "plain": true}); err != nil {
						return err
					}
				}
			}
			if p := sb.EndCardPreview; p != nil && p.Key != "" {
				if err := r.recordAsset(ctx, tx, v, AssetKeyframe, p.Key, p.URL, map[string]any{"end_card_preview": true}); err != nil {
					return err
				}
			}
			if sb.Actor != nil && sb.Actor.PlainKey != "" {
				if err := r.recordAsset(ctx, tx, v, AssetKeyframe, sb.Actor.PlainKey, sb.Actor.PlainURL, map[string]any{"actor": true, "plain": true}); err != nil {
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
		// A consent that ended while the storyboard was drawn: the actor is not kept for other
		// videos, the gates refuse this one, and the hourly sweep deletes the files (faces.go).
		agreed := sb.Actor == nil || sb.Actor.FaceID == ""
		if !agreed {
			_, agreed = liveFace(ctx, r.db, v.TenantID, sb.Actor.FaceID, r.nowFn)
		}
		if sb.Actor != nil && sb.Actor.URL != "" && agreed {
			r.keepActor(ctx, v.BriefID, sb.Actor, redraw)
		}
		if settled {
			// A WhatsApp turn's video: the stills go to the requester with the render code.
			r.notifyOSTurn(ctx, v, OSTurnStoryboardReady, stillLinks(sb.Stills), "")
			// An autopilot video in veto-window mode: the agent approves the stills and renders.
			r.notifyAutopilot(ctx, v, AutopilotStoryboardReady)
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

// keepActor stores the actor on the brief. An existing actor is replaced only by a redraw the
// owner asked for (replace), so the brief's later versions get the new presenter.
func (r *Renderer) keepActor(ctx context.Context, briefID uuid.UUID, actor *storyboardActor, replace bool) {
	b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(briefID)).Only(ctx)
	if err != nil {
		return
	}
	if old, ok := b.Metadata["actor"].(map[string]any); ok && str(old["url"]) != "" && !replace {
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

// endCardPreviewCall is marketflow-ai's POST /api/v1/internal/creative/end-card-preview body.
type endCardPreviewCall struct {
	TenantID  string `json:"tenant_id"`
	VersionID string `json:"version_id"`
	Format    string `json:"format"`
	SceneID   string `json:"scene_id"`
	StillURL  string `json:"still_url"`
	endCardWords
}

// RecomposeEndCardPreview lays the end card's current words, contacts and logo plate over the
// end card still the storyboard already drew (owner, 2026-11-24): nothing is redrawn and nothing
// is charged, and an approval stands, since the plan's fingerprint covers the stills, not the
// preview. Only a video ad script waiting for review with a storyboard has a preview to recompose.
func (r *Renderer) RecomposeEndCardPreview(ctx context.Context, v *ent.CreativeVersion) (*ent.CreativeVersion, error) {
	sb, ok := v.Metadata["storyboard"].(map[string]any)
	if v.Kind != KindVideoAd || v.Status != StatusScriptReady || !ok {
		return nil, fmt.Errorf("%w: only a video ad with a storyboard waiting for review has an end card preview", ErrConflict)
	}
	var script VideoScript
	if b, err := json.Marshal(v.Copy["script"]); err != nil || json.Unmarshal(b, &script) != nil {
		return nil, fmt.Errorf("%w: this video ad has no script", ErrConflict)
	}
	call := endCardPreviewCall{TenantID: v.TenantID.String(), VersionID: v.ID.String(), Format: v.Format}
	for _, sc := range script.Scenes {
		if sc.Shot == "end_card" {
			call.SceneID = sc.ID
		}
	}
	if stills, ok := sb["stills"].([]any); ok && call.SceneID != "" {
		for _, x := range stills {
			if st, ok := x.(map[string]any); ok && str(st["scene_id"]) == call.SceneID && str(st["url"]) != "" {
				call.StillURL = str(st["url"])
			}
		}
	}
	if call.StillURL == "" {
		return nil, fmt.Errorf("%w: the storyboard has no end card still; make the storyboard first", ErrConflict)
	}
	b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx)
	if err != nil {
		b = nil
	}
	var offering map[string]any
	if b != nil {
		offering = b.Offering
	}
	call.endCardWords = r.endCardWordsFor(ctx, v, b, r.resolvePlace(ctx, v.TenantID, offering, false))
	data, status, err := r.aiCall(ctx, http.MethodPost, "/api/v1/internal/creative/end-card-preview", call, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, aiStatusError(status, data)
	}
	var reply struct {
		Preview *struct {
			SceneID string `json:"scene_id"`
			URL     string `json:"url"`
			Key     string `json:"key"`
		} `json:"end_card_preview"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Preview == nil || reply.Preview.URL == "" {
		return nil, errors.New("marketflow-ai sent an unreadable end card preview")
	}
	p := reply.Preview
	meta := copyMap(v.Metadata)
	board := copyMap(sb)
	board["end_card_preview"] = map[string]any{"scene_id": p.SceneID, "url": p.URL, "key": p.Key}
	meta["storyboard"] = board
	err = withTx(ctx, r.db, func(tx *ent.Tx) error {
		n, err := tx.CreativeVersion.Update().
			Where(creativeversion.ID(v.ID), creativeversion.StatusEQ(StatusScriptReady), creativeversion.UpdatedAt(v.UpdatedAt)).
			SetMetadata(meta).Save(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: the version changed meanwhile; try again", ErrConflict)
		}
		if p.Key == "" {
			return nil
		}
		return r.recordAsset(ctx, tx, v, AssetKeyframe, p.Key, p.URL, map[string]any{"end_card_preview": true})
	})
	if err != nil {
		return nil, err
	}
	return r.db.CreativeVersion.Get(ctx, v.ID)
}
