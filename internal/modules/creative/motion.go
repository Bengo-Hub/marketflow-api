package creative

import (
	"context"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/google/uuid"
)

// A motion poster is a short clip made from a finished poster's scene, with the
// poster's words laid over it by marketflow-ai. It runs as a media job there
// (jobs.go): the renderer starts it, and the outcome is pushed back.

const (
	minClipSeconds = 4
	maxClipSeconds = 10
)

// MotionInput asks for a motion poster of a finished poster.
type MotionInput struct {
	Prompt  string `json:"prompt"`  // the motion, optional ("slow push in, steam rising")
	Seconds int    `json:"seconds"` // 4 to 10, default 5
	Tier    string `json:"tier"`    // standard (default) or premium
}

func (in *MotionInput) validate() error {
	in.Prompt = strings.TrimSpace(in.Prompt)
	if len([]rune(in.Prompt)) > 1000 {
		return invalid("the motion description is limited to 1000 characters")
	}
	if in.Seconds == 0 {
		in.Seconds = 5
	}
	if in.Seconds < minClipSeconds || in.Seconds > maxClipSeconds {
		return invalid(fmt.Sprintf("seconds must be %d to %d", minClipSeconds, maxClipSeconds))
	}
	if in.Tier == "" {
		in.Tier = "standard"
	}
	if !tiers[in.Tier] {
		return invalid("tier must be standard or premium")
	}
	return nil
}

// CreateMotion queues a motion poster of a finished poster: same scene, same
// words, same format. It is a paid render and counts toward the daily cap.
func (s *Service) CreateMotion(ctx context.Context, tenantID, sourceID uuid.UUID, in MotionInput) (*ent.CreativeVersion, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	src, err := s.version(ctx, tenantID, sourceID)
	if err != nil {
		return nil, err
	}
	scene, err := finishedScene(src, "animated")
	if err != nil {
		return nil, err
	}
	if err := s.checkLiveDrafts(ctx, tenantID); err != nil {
		return nil, err
	}
	if err := s.checkDailyCap(ctx, tenantID); err != nil {
		return nil, err
	}
	meta := map[string]any{"scene_url": scene, "seconds": in.Seconds}
	if in.Prompt != "" {
		meta["motion_prompt"] = in.Prompt
	}
	if cats := restrictedCategories(str(src.Copy["prompt"]), in.Prompt, str(src.Copy["headline"]), str(src.Copy["subline"]), str(src.Copy["cta"])); len(cats) > 0 {
		meta["restricted_categories"] = cats
	}
	return s.insertVersion(ctx, tenantID, src.BriefID, &src.ID, KindVideo, in.Tier, src.Format, copyMap(src.Copy), meta)
}

// clipCall is marketflow-ai's POST /api/v1/internal/creative/clip body.
type clipCall struct {
	TenantID  string `json:"tenant_id"`
	VersionID string `json:"version_id"`
	SceneURL  string `json:"scene_url"`
	Prompt    string `json:"prompt,omitempty"`
	Format    string `json:"format"`
	Seconds   int    `json:"seconds"`
	Tier      string `json:"tier"`
	Accent    string `json:"accent,omitempty"`
	LogoURL   string `json:"logo_url,omitempty"`
	Headline  string `json:"headline,omitempty"`
	Subline   string `json:"subline,omitempty"`
	Price     string `json:"price,omitempty"`
	CTA       string `json:"cta,omitempty"`
	Charge    bool   `json:"charge"`
}

// startClip starts a claimed motion poster's job in marketflow-ai.
func (r *Renderer) startClip(ctx context.Context, v *ent.CreativeVersion) {
	meta := copyMap(v.Metadata)
	call := clipCall{TenantID: v.TenantID.String(), VersionID: v.ID.String(), SceneURL: str(meta["scene_url"]),
		Prompt: str(meta["motion_prompt"]), Format: v.Format, Seconds: intOf(meta["seconds"]), Tier: v.Tier,
		Headline: str(v.Copy["headline"]), Subline: str(v.Copy["subline"]), Price: str(v.Copy["price"]),
		CTA: str(v.Copy["cta"]), Charge: true}
	if b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx); err == nil {
		brand, _ := b.Metadata["brand"].(map[string]any)
		call.Accent, call.LogoURL = str(brand["accent"]), str(brand["logo_url"])
	}
	r.startJob(ctx, v, meta, "/api/v1/internal/creative/clip", call)
}
