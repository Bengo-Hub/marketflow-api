package creative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
)

// The generation plan gate (owner, 2026-11-19). Before any video render starts, the owner sees
// and approves, in order: the presenter preview (the storyboard's actor), the budget (the quote),
// the script and its scenes, and the generation plan: the exact prompt and start frame each
// provider will receive, which marketflow-ai returns with the quote. The plan is named by a
// fingerprint of everything that shapes it, so the render refuses (409) until the owner approved
// the plan as it is now: any change to the script, the end card, the storyboard or the video's
// settings gives a new fingerprint and asks for the approval again. The approval rides on the
// existing storyboard approval (POST /storyboard/approve with plan_fingerprint), so there is one
// path, not two.

// metaGenerationPlan is the version metadata key of the approved plan.
const metaGenerationPlan = "generation_plan"

// planInputs is everything that shapes the generation plan.
type planInputs struct {
	Format   string         `json:"format"`
	Kind     string         `json:"kind"`
	Tier     string         `json:"tier"`
	Settings map[string]any `json:"settings"`
	Script   any            `json:"script"`
	EndCard  any            `json:"end_card"`
	Photos   []string       `json:"photos"`
	Brand    []string       `json:"brand_images"`
	Actor    string         `json:"actor"`
	Stills   []string       `json:"stills"`
}

// stableLink is a link without its query: signed links change on every read.
func stableLink(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// PlanFingerprint names a version's generation plan as it is now.
func PlanFingerprint(v *ent.CreativeVersion) string {
	in := planInputs{Format: v.Format, Kind: videoKind(v), Tier: v.Tier, Settings: map[string]any{},
		Script: v.Copy["script"], EndCard: v.Copy["end_card"], Photos: strs(v.Metadata["photo_urls"]),
		Brand: strs(v.Metadata[metaBrandImageKeys])}
	for _, k := range []string{"delivery", "voice", "voice_gender", "voice_accent", "language", "music_url", "seconds"} {
		if val, ok := v.Metadata[k]; ok {
			in.Settings[k] = val
		}
	}
	if sb, ok := v.Metadata["storyboard"].(map[string]any); ok {
		if actor, ok := sb["actor"].(map[string]any); ok {
			in.Actor = stableLink(str(actor["url"]))
		}
		if stills, ok := sb["stills"].([]any); ok {
			for _, x := range stills {
				if st, ok := x.(map[string]any); ok {
					in.Stills = append(in.Stills, str(st["scene_id"])+"|"+str(st["source"])+"|"+stableLink(str(st["url"])))
				}
			}
		}
	}
	b, _ := json.Marshal(in) // map keys are sorted, so the fingerprint is stable
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}

// planApproved reports whether the owner approved the version's generation plan as it is now.
func planApproved(v *ent.CreativeVersion) bool {
	gp, ok := v.Metadata[metaGenerationPlan].(map[string]any)
	return ok && str(gp["approved_at"]) != "" && str(gp["fingerprint"]) == PlanFingerprint(v)
}

// checkPlanForRender is the render's gate.
func checkPlanForRender(v *ent.CreativeVersion) error {
	if planApproved(v) {
		return nil
	}
	if gp, ok := v.Metadata[metaGenerationPlan].(map[string]any); ok && str(gp["approved_at"]) != "" {
		return fmt.Errorf("%w: the script, storyboard or settings changed after the generation plan was approved; review the plan and approve it again", ErrConflict)
	}
	return fmt.Errorf("%w: review and approve the generation plan (the prompts and start frames of every scene) before rendering", ErrConflict)
}

// errPlanChanged is an approval of a plan that is no longer the version's.
var errPlanChanged = fmt.Errorf("%w: the plan changed since it was shown; look at it again before approving", ErrConflict)

// planScenes is the part of a quote's plan kept with the approval: what each scene is sent.
func planScenes(quote json.RawMessage) []any {
	var q struct {
		Plan struct {
			Scenes []map[string]any `json:"scenes"`
		} `json:"plan"`
	}
	if json.Unmarshal(quote, &q) != nil {
		return nil
	}
	out := make([]any, 0, len(q.Plan.Scenes))
	for _, s := range q.Plan.Scenes {
		keep := map[string]any{}
		for _, k := range []string{"scene_id", "shot", "work", "route", "frame", "frame_url", "photo", "origin", "prompt",
			"negative_prompt", "still_prompt", "cached", "cost_usd"} {
			if val, ok := s[k]; ok {
				keep[k] = val
			}
		}
		out = append(out, keep)
	}
	return out
}

// missingStill names the first scene of a quote's plan that needs a storyboard still it does not
// have yet ("" when none).
func missingStill(quote json.RawMessage) string {
	var q struct {
		Plan struct {
			Scenes []struct {
				SceneID  string `json:"scene_id"`
				Frame    string `json:"frame"`
				FrameURL string `json:"frame_url"`
			} `json:"scenes"`
		} `json:"plan"`
	}
	if json.Unmarshal(quote, &q) != nil {
		return ""
	}
	for _, s := range q.Plan.Scenes {
		if s.Frame == "generated" && s.FrameURL == "" {
			return s.SceneID
		}
	}
	return ""
}

// withPlanFingerprint adds the version's plan fingerprint and approval to marketflow-ai's quote,
// so the Studio and WhatsApp approve exactly the plan they show.
func withPlanFingerprint(quote json.RawMessage, v *ent.CreativeVersion) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(quote, &m) != nil || m == nil {
		return quote
	}
	m["plan_fingerprint"] = PlanFingerprint(v)
	m["plan_approved"] = planApproved(v)
	b, err := json.Marshal(m)
	if err != nil {
		return quote
	}
	return b
}

// ApprovePlan records the approval of the generation plan named by fingerprint, with the scenes
// of the quote that showed it (nil when no quote could be read). by marks who approved it.
func (s *Service) ApprovePlan(ctx context.Context, tenantID, id uuid.UUID, fingerprint string, scenes []any,
	mark func(map[string]any)) (*ent.CreativeVersion, error) {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if v.Kind != KindVideoAd || v.Status != StatusScriptReady {
		return nil, fmt.Errorf("%w: only a video ad waiting for review has a plan to approve", ErrConflict)
	}
	if fingerprint != PlanFingerprint(v) {
		return nil, errPlanChanged
	}
	if isPresenter(videoKind(v)) && !storyboardApproved(v) {
		return nil, fmt.Errorf("%w: approve the presenter and the storyboard before the plan", ErrConflict)
	}
	meta := copyMap(v.Metadata)
	gp := map[string]any{"fingerprint": fingerprint, "approved_at": s.nowFn().UTC().Format(time.RFC3339)}
	if len(scenes) > 0 {
		gp["scenes"] = scenes
	}
	if mark != nil {
		mark(gp)
	}
	meta[metaGenerationPlan] = gp
	n, err := s.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.TenantID(tenantID), creativeversion.StatusEQ(StatusScriptReady),
			creativeversion.UpdatedAtEQ(v.UpdatedAt)).
		SetMetadata(meta).Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errPlanChanged
	}
	return s.version(ctx, tenantID, id)
}
