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

// A template video ad is made in two steps, so the owner sees and can edit the
// words before any render is paid for:
//  1. script: the worker asks marketflow-ai's Content Creator for a script
//     (1 credit, reviewed for compliance there); the version waits as
//     script_ready with the script in its copy;
//  2. render: after the owner's edits, the worker starts marketflow-ai's ad
//     job (the voice-over, the owner's photos and footage, captions, the end
//     card), and the result is pushed back like a motion poster's (jobs.go).
// Everything lives in the version's copy and metadata: no schema change.

// Video ad stages (metadata.stage).
const (
	StageScript     = "script"
	StageStoryboard = "storyboard"
	StageRender     = "render"
)

// Video ad kinds (metadata.video_kind): the studio's video tiers.
const (
	VideoTemplate         = "template"
	VideoPresenter        = "presenter"
	VideoPresenterPremium = "presenter_premium"
)

// videoKind is a video ad's kind; ads made before kinds are template ads.
func videoKind(v *ent.CreativeVersion) string {
	if k := str(v.Metadata["video_kind"]); k != "" {
		return k
	}
	return VideoTemplate
}

func isPresenter(kind string) bool { return kind == VideoPresenter || kind == VideoPresenterPremium }

// Video ad limits.
const (
	minAdSeconds      = 15
	maxAdSeconds      = 60
	maxAdPhotos       = 8
	maxAdFootage      = 4
	maxAdScenes       = 12
	maxAdVoiceChars   = 900 // marketflow-ai's limit for about 60 seconds
	maxSceneVoice     = 400
	maxSceneCaption   = 120
	maxPresenterVoice = 200 // marketflow-ai's limit for one presenter scene (about 14 s)
	maxSceneVisual    = 300
	scriptCallTimeout = 110 * time.Second // marketflow-ai answers within 90 s
)

var (
	adFormats    = map[string]bool{"1080x1080": true, "1080x1350": true, "1080x1920": true}
	adDeliveries = map[string]bool{"": true, "ad": true, "ugc": true, "kids": true, "story": true, "info": true}
	// adShots are the shots a template ad renders; a presenter ad adds "presenter".
	adShots    = map[string]bool{"product_clip": true, "b_roll": true, "owner_footage": true, "end_card": true}
	videoKinds = map[string]bool{"": true, VideoTemplate: true, VideoPresenter: true, VideoPresenterPremium: true}
)

// stage is a video ad version's step; a version made before stages counts as
// the script step.
func stage(v *ent.CreativeVersion) string {
	if s := str(v.Metadata["stage"]); s != "" {
		return s
	}
	return StageScript
}

// VideoAdInput asks for a template video ad of a brief's offering.
type VideoAdInput struct {
	Format      string   `json:"format"`   // default 1080x1920 (Reels, TikTok, Status)
	Seconds     int      `json:"seconds"`  // 15 to 60, default 30
	Language    string   `json:"language"` // en or sw; default the brief's
	Tier        string   `json:"tier"`     // standard (default) or premium
	Voice       string   `json:"voice"`    // a voice id or name; "" picks the best for the delivery
	Delivery    string   `json:"delivery"` // ad (default) | ugc | kids | story | info
	VoiceGender string   `json:"voice_gender"`
	VoiceAccent string   `json:"voice_accent"` // e.g. african, kenyan, nigerian
	// Kind is the video type: template (default), presenter or presenter_premium.
	Kind string `json:"kind"`
	// ActorPrompt describes the presenter for a new actor (presenter types); the brief's
	// actor is kept once it exists.
	ActorPrompt string   `json:"actor_prompt"`
	PhotoURLs   []string `json:"photo_urls"` // default: the offering's photos
	FootageURLs []string `json:"footage_urls"` // the owner's clips
	// The end card's words; empty fields come from the offering.
	Headline string `json:"headline"`
	Subline  string `json:"subline"`
	Price    string `json:"price"`
	CTA      string `json:"cta"`
	// MusicURL is an optional music bed under the voice. MusicLicensed is the owner's
	// confirmation that the track may be used in ads; a link without it is refused.
	MusicURL      string `json:"music_url"`
	MusicLicensed bool   `json:"music_licensed"`
}

func (in *VideoAdInput) validate(brief *ent.CreativeBrief) error {
	if in.Format == "" {
		in.Format = "1080x1920"
	}
	if !adFormats[in.Format] {
		return invalid("format must be 1080x1080, 1080x1350 or 1080x1920")
	}
	if in.Seconds == 0 {
		in.Seconds = 30
	}
	if in.Seconds < minAdSeconds || in.Seconds > maxAdSeconds {
		return invalid(fmt.Sprintf("seconds must be %d to %d", minAdSeconds, maxAdSeconds))
	}
	if in.Language == "" {
		in.Language = brief.Language
	}
	if !languages[in.Language] {
		return invalid("language must be en or sw")
	}
	if in.Tier == "" {
		in.Tier = "standard"
	}
	if !tiers[in.Tier] {
		return invalid("tier must be standard or premium")
	}
	if !videoKinds[in.Kind] {
		return invalid("kind must be template, presenter or presenter_premium")
	}
	if in.Kind == "" {
		in.Kind = VideoTemplate
	}
	if in.Kind == VideoPresenterPremium {
		in.Tier = "premium"
	}
	if len([]rune(in.ActorPrompt)) > 300 {
		return invalid("actor_prompt is up to 300 characters")
	}
	if !adDeliveries[in.Delivery] {
		return invalid("delivery must be ad, ugc, kids, story or info")
	}
	if in.VoiceGender != "" && in.VoiceGender != "female" && in.VoiceGender != "male" {
		return invalid("voice_gender must be female or male")
	}
	if len(in.Voice) > 100 || len(in.VoiceAccent) > 40 {
		return invalid("voice or voice_accent is too long")
	}
	if len(in.PhotoURLs) == 0 {
		in.PhotoURLs = strs(brief.Offering["photo_urls"])
	}
	if len(in.PhotoURLs) > maxAdPhotos || len(in.FootageURLs) > maxAdFootage {
		return invalid(fmt.Sprintf("at most %d photos and %d clips", maxAdPhotos, maxAdFootage))
	}
	if len(in.PhotoURLs)+len(in.FootageURLs) == 0 {
		return invalid("add at least one product photo or clip")
	}
	for _, u := range append(append([]string{}, in.PhotoURLs...), in.FootageURLs...) {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return invalid("photo_urls and footage_urls must be http(s) links")
		}
	}
	for _, f := range []*string{&in.Headline, &in.Subline, &in.Price, &in.CTA} {
		*f = strings.TrimSpace(*f)
		if len(*f) > maxCopyLen {
			return invalid(fmt.Sprintf("end card text is limited to %d characters per line", maxCopyLen))
		}
	}
	if in.MusicURL = strings.TrimSpace(in.MusicURL); in.MusicURL != "" {
		if !strings.HasPrefix(in.MusicURL, "https://") || len(in.MusicURL) > 1000 {
			return invalid("music_url must be an https link")
		}
		if !in.MusicLicensed {
			return invalid("confirm you have the right to use this music in ads")
		}
	}
	return nil
}

// CreateVideoAd queues a video ad's script step. Rendering is a separate,
// paid step after the owner has seen the script (RenderVideoAd).
func (s *Service) CreateVideoAd(ctx context.Context, tenantID, briefID uuid.UUID, in VideoAdInput) (*ent.CreativeVersion, error) {
	brief, err := s.brief(ctx, tenantID, briefID)
	if err != nil {
		return nil, err
	}
	if err := in.validate(brief); err != nil {
		return nil, err
	}
	if err := s.checkLiveDrafts(ctx, tenantID); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(str(brief.Offering["name"]))
	if name == "" {
		name = brief.Title
	}
	card := map[string]any{"headline": firstNonEmpty(in.Headline, name), "subline": in.Subline,
		"price": firstNonEmpty(in.Price, offeringPrice(brief.Offering)), "cta": in.CTA}
	meta := map[string]any{"stage": StageScript, "seconds": in.Seconds, "language": in.Language,
		"photo_urls": in.PhotoURLs, "footage_urls": in.FootageURLs, "video_kind": in.Kind}
	for k, v := range map[string]string{"voice": in.Voice, "delivery": in.Delivery, "voice_gender": in.VoiceGender,
		"voice_accent": in.VoiceAccent, "actor_prompt": strings.TrimSpace(in.ActorPrompt), "music_url": in.MusicURL} {
		if v != "" {
			meta[k] = v
		}
	}
	if in.MusicURL != "" {
		meta["music_licensed_confirmed"] = true
	}
	return s.insertVersion(ctx, tenantID, briefID, nil, KindVideoAd, in.Tier, in.Format, map[string]any{"end_card": card}, meta)
}

// ScriptInput is the owner's edit of a video ad's script and end card. Nil
// fields keep what is there.
type ScriptInput struct {
	Script  *VideoScript   `json:"script"`
	EndCard map[string]any `json:"end_card"`
}

// VideoScript is marketflow-ai's video_script contract as the studio stores it.
type VideoScript struct {
	Format string        `json:"format"`
	Hooks  []string      `json:"hooks"`
	Scenes []ScriptScene `json:"scenes"`
	CTA    string        `json:"cta"`
}

// ScriptScene is one scene of a script.
type ScriptScene struct {
	ID        string  `json:"id"`
	DurationS float64 `json:"duration_s"`
	Shot      string  `json:"shot"`
	Asset     int     `json:"asset,omitempty"`
	Visual    string  `json:"visual"`
	Voiceover string  `json:"voiceover"`
	Caption   string  `json:"caption"`
}

func (v *VideoScript) validate(photos, footage int, kind string) error {
	if len(v.Scenes) < 2 || len(v.Scenes) > maxAdScenes {
		return invalid(fmt.Sprintf("a script has 2 to %d scenes", maxAdScenes))
	}
	chars := 0
	for i := range v.Scenes {
		sc := &v.Scenes[i]
		sc.Voiceover, sc.Caption = strings.TrimSpace(sc.Voiceover), strings.TrimSpace(sc.Caption)
		sc.Visual = strings.TrimSpace(sc.Visual)
		if sc.Shot == "presenter" {
			if !isPresenter(kind) {
				return invalid(fmt.Sprintf("scene %d: a presenter scene needs the presenter video type", i+1))
			}
			if sc.Voiceover == "" || utf8.RuneCountInString(sc.Voiceover) > maxPresenterVoice {
				return invalid(fmt.Sprintf("scene %d: a presenter scene says 1 to %d characters", i+1, maxPresenterVoice))
			}
			if utf8.RuneCountInString(sc.Visual) > maxSceneVisual {
				return invalid(fmt.Sprintf("scene %d: the visual is up to %d characters", i+1, maxSceneVisual))
			}
			if sc.ID == "" {
				sc.ID = fmt.Sprintf("s%d", i+1)
			}
			chars += utf8.RuneCountInString(sc.Voiceover)
			continue
		}
		if !adShots[sc.Shot] {
			return invalid(fmt.Sprintf("scene %d: shot must be presenter, product_clip, b_roll, owner_footage or end_card", i+1))
		}
		if utf8.RuneCountInString(sc.Visual) > maxSceneVisual {
			return invalid(fmt.Sprintf("scene %d: the visual is up to %d characters", i+1, maxSceneVisual))
		}
		limit := photos
		if sc.Shot == "owner_footage" {
			limit = footage
		}
		if sc.Asset < 0 || sc.Asset > limit || (sc.Shot == "owner_footage" && footage == 0) || (sc.Shot != "owner_footage" && photos == 0) {
			return invalid(fmt.Sprintf("scene %d uses a photo or clip this ad does not have", i+1))
		}
		if utf8.RuneCountInString(sc.Voiceover) > maxSceneVoice || utf8.RuneCountInString(sc.Caption) > maxSceneCaption {
			return invalid(fmt.Sprintf("scene %d: voiceover up to %d characters, caption up to %d", i+1, maxSceneVoice, maxSceneCaption))
		}
		if sc.ID == "" {
			sc.ID = fmt.Sprintf("s%d", i+1)
		}
		chars += utf8.RuneCountInString(sc.Voiceover)
	}
	if chars == 0 || chars > maxAdVoiceChars {
		return invalid(fmt.Sprintf("the voice-over is 1 to %d characters in all (about 60 seconds)", maxAdVoiceChars))
	}
	return nil
}

// UpdateScript stores the owner's edits of a script waiting for review. The
// restricted-category flags are worked out again over the new words;
// marketflow-ai checks compliance once more before it renders.
func (s *Service) UpdateScript(ctx context.Context, tenantID, id uuid.UUID, in ScriptInput) (*ent.CreativeVersion, error) {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if v.Kind != KindVideoAd || v.Status != StatusScriptReady {
		return nil, fmt.Errorf("%w: only a video ad script waiting for review can be edited", ErrConflict)
	}
	copyFields := copyMap(v.Copy)
	if in.Script != nil {
		if err := in.Script.validate(len(strs(v.Metadata["photo_urls"])), len(strs(v.Metadata["footage_urls"])), videoKind(v)); err != nil {
			return nil, err
		}
		copyFields["script"] = toMap(in.Script)
	}
	if in.EndCard != nil {
		card := map[string]any{}
		for _, k := range []string{"headline", "subline", "price", "cta"} {
			t := strings.TrimSpace(str(in.EndCard[k]))
			if len(t) > maxCopyLen {
				return nil, invalid(fmt.Sprintf("end card text is limited to %d characters per line", maxCopyLen))
			}
			card[k] = t
		}
		copyFields["end_card"] = card
	}
	meta := copyMap(v.Metadata)
	meta["script_edited_at"] = s.nowFn().UTC().Format(time.RFC3339)
	// New words or scenes need a fresh look at the storyboard before a presenter render.
	if sb, ok := meta["storyboard"].(map[string]any); ok && in.Script != nil {
		sb = copyMap(sb)
		delete(sb, "approved_at")
		delete(sb, "approved_by")
		meta["storyboard"] = sb
	}
	if cats := restrictedCategories(scriptText(copyFields)...); len(cats) > 0 {
		meta["restricted_categories"] = cats
	} else {
		delete(meta, "restricted_categories")
	}
	n, err := s.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.TenantID(tenantID), creativeversion.StatusEQ(StatusScriptReady)).
		SetCopy(copyFields).SetMetadata(meta).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: the script is no longer waiting for review", ErrConflict)
	}
	return s.version(ctx, tenantID, id)
}

// RenderVideoAd queues the paid render of a reviewed script. It counts toward
// the daily render cap.
func (s *Service) RenderVideoAd(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if v.Kind != KindVideoAd || v.Status != StatusScriptReady {
		return nil, fmt.Errorf("%w: only a video ad script waiting for review can be rendered", ErrConflict)
	}
	if _, ok := v.Copy["script"]; !ok {
		return nil, fmt.Errorf("%w: this video ad has no script", ErrConflict)
	}
	if isPresenter(videoKind(v)) && !storyboardApproved(v) {
		return nil, fmt.Errorf("%w: approve the storyboard before rendering a presenter ad", ErrConflict)
	}
	if err := s.checkDailyCap(ctx, tenantID); err != nil {
		return nil, err
	}
	meta := copyMap(v.Metadata)
	meta["stage"] = StageRender
	delete(meta, "attempts") // the render's attempts count from zero
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

// scriptCall is marketflow-ai's POST /api/v1/internal/creative/script body.
type scriptCall struct {
	TenantID  string `json:"tenant_id"`
	VersionID string `json:"version_id"`
	Offering  string `json:"offering"`
	Facts     string `json:"facts"`
	Objective string `json:"objective"`
	Audience  string `json:"audience"`
	Language  string `json:"language"`
	Seconds   int    `json:"seconds"`
	Photos    int    `json:"photos"`
	Footage   int    `json:"footage"`
	Kind      string `json:"kind"`
	Charge    bool   `json:"charge"`
}

// writeScript runs a claimed video ad's script step: marketflow-ai writes and
// reviews the script synchronously, and the version waits for the owner.
func (r *Renderer) writeScript(ctx context.Context, v *ent.CreativeVersion) {
	meta := copyMap(v.Metadata)
	meta["attempts"] = intOf(meta["attempts"]) + 1
	_, _ = r.db.CreativeVersion.UpdateOneID(v.ID).SetMetadata(meta).Save(ctx)

	b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx)
	if err != nil {
		r.failJob(ctx, v.ID, err)
		return
	}
	name := strings.TrimSpace(str(b.Offering["name"]))
	if name == "" {
		name = b.Title
	}
	call := scriptCall{TenantID: v.TenantID.String(), VersionID: v.ID.String(), Offering: excerpt(name, 300),
		Facts: offeringFacts(b.Offering), Objective: b.Objective, Audience: b.Audience, Language: str(meta["language"]),
		Seconds: intOf(meta["seconds"]), Photos: len(strs(meta["photo_urls"])), Footage: len(strs(meta["footage_urls"])),
		Kind: videoKind(v), Charge: true}
	data, status, err := r.aiCall(ctx, http.MethodPost, "/api/v1/internal/creative/script", call, scriptCallTimeout)
	if err == nil && status != http.StatusOK {
		err = aiStatusError(status, data)
		if status == http.StatusUnprocessableEntity {
			// The review blocked the script: keep its findings for the owner.
			var blocked struct {
				Findings []map[string]any `json:"findings"`
			}
			if json.Unmarshal(data, &blocked) == nil && len(blocked.Findings) > 0 {
				meta["findings"] = blocked.Findings
				_, _ = r.db.CreativeVersion.UpdateOneID(v.ID).SetMetadata(meta).Save(ctx)
			}
		}
	}
	var reply struct {
		Script         *VideoScript     `json:"script"`
		Findings       []map[string]any `json:"findings"`
		CreditsCharged int              `json:"credits_charged"`
		CostUSD        float64          `json:"cost_usd"`
	}
	if err == nil {
		if jerr := json.Unmarshal(data, &reply); jerr != nil || reply.Script == nil || len(reply.Script.Scenes) == 0 {
			err = errors.New("marketflow-ai sent an unreadable script")
		}
	}
	if err != nil {
		r.failJob(ctx, v.ID, err)
		return
	}
	copyFields := copyMap(v.Copy)
	copyFields["script"] = toMap(reply.Script)
	meta["script_cost_usd"] = reply.CostUSD
	if len(reply.Findings) > 0 {
		meta["findings"] = reply.Findings
	}
	if cats := restrictedCategories(append(scriptText(copyFields), b.Title, name)...); len(cats) > 0 {
		meta["restricted_categories"] = cats
	}
	if err := r.db.CreativeVersion.Update().
		Where(creativeversion.ID(v.ID), creativeversion.StatusEQ(StatusRendering)).
		SetStatus(StatusScriptReady).SetCopy(copyFields).SetMetadata(meta).
		SetCredits(v.Credits + reply.CreditsCharged).ClearError().
		Exec(ctx); err != nil {
		r.logger.Error("script written but not recorded", zap.String("version_id", v.ID.String()), zap.Error(err))
		return
	}
	r.logger.Info("video ad script ready", zap.String("version_id", v.ID.String()))
}

// adCall is marketflow-ai's POST /api/v1/internal/creative/ad body.
type adCall struct {
	TenantID    string        `json:"tenant_id"`
	VersionID   string        `json:"version_id"`
	Format      string        `json:"format"`
	Tier        string        `json:"tier"`
	Voice       string        `json:"voice,omitempty"`
	Delivery    string        `json:"delivery,omitempty"`
	VoiceGender string        `json:"voice_gender,omitempty"`
	VoiceAccent string        `json:"voice_accent,omitempty"`
	Language    string        `json:"language"`
	Scenes      []ScriptScene `json:"scenes"`
	PhotoURLs   []string      `json:"photo_urls"`
	FootageURLs []string      `json:"footage_urls,omitempty"`
	Accent      string        `json:"accent,omitempty"`
	LogoURL     string        `json:"logo_url,omitempty"`
	MusicURL    string        `json:"music_url,omitempty"`
	Headline    string        `json:"headline,omitempty"`
	Subline     string        `json:"subline,omitempty"`
	Price       string        `json:"price,omitempty"`
	CTA         string        `json:"cta,omitempty"`
	Charge      bool          `json:"charge"`
	// Presenter ads: the approved storyboard's actor and stills, and the scene cache of
	// the version this one revises.
	Kind       string            `json:"kind,omitempty"`
	ActorURL   string            `json:"actor_url,omitempty"`
	Seed       int64             `json:"seed,omitempty"`
	Stills     map[string]string `json:"stills,omitempty"`
	SceneCache []any             `json:"scene_cache,omitempty"`
}

// startAd starts a claimed video ad's render job in marketflow-ai.
func (r *Renderer) startAd(ctx context.Context, v *ent.CreativeVersion) {
	meta := copyMap(v.Metadata)
	var script VideoScript
	if b, err := json.Marshal(v.Copy["script"]); err != nil || json.Unmarshal(b, &script) != nil || len(script.Scenes) == 0 {
		r.failJob(ctx, v.ID, &aiError{msg: "this video ad has no script; write it again"})
		return
	}
	call := r.adCallFor(ctx, v, script, true)
	r.startJob(ctx, v, meta, "/api/v1/internal/creative/ad", call)
}

// adCallFor builds marketflow-ai's ad body for a version (the render, or its quote).
func (r *Renderer) adCallFor(ctx context.Context, v *ent.CreativeVersion, script VideoScript, charge bool) adCall {
	meta := v.Metadata
	card, _ := v.Copy["end_card"].(map[string]any)
	call := adCall{TenantID: v.TenantID.String(), VersionID: v.ID.String(), Format: v.Format, Tier: v.Tier,
		Voice: str(meta["voice"]), Delivery: str(meta["delivery"]), VoiceGender: str(meta["voice_gender"]),
		VoiceAccent: str(meta["voice_accent"]), Language: str(meta["language"]), Scenes: script.Scenes,
		PhotoURLs: strs(meta["photo_urls"]), FootageURLs: strs(meta["footage_urls"]),
		Headline: str(card["headline"]), Subline: str(card["subline"]), Price: str(card["price"]), CTA: str(card["cta"]),
		Charge: charge, Kind: videoKind(v), MusicURL: str(meta["music_url"])}
	if b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx); err == nil {
		brand, _ := b.Metadata["brand"].(map[string]any)
		call.Accent, call.LogoURL = str(brand["accent"]), str(brand["logo_url"])
	}
	if sb, ok := meta["storyboard"].(map[string]any); ok && isPresenter(call.Kind) {
		if actor, ok := sb["actor"].(map[string]any); ok {
			call.ActorURL = str(actor["url"])
			if f, ok := actor["seed"].(float64); ok {
				call.Seed = int64(f)
			}
		}
		call.Stills = map[string]string{}
		if stills, ok := sb["stills"].([]any); ok {
			for _, x := range stills {
				if st, ok := x.(map[string]any); ok && str(st["source"]) == "generated" && str(st["url"]) != "" {
					call.Stills[str(st["scene_id"])] = str(st["url"])
				}
			}
		}
	}
	if cache, ok := meta["scene_cache"].([]any); ok {
		call.SceneCache = cache
	}
	return call
}

// offeringFacts is what the script may say about the offering: its own
// snapshot, nothing generated.
func offeringFacts(o map[string]any) string {
	var lines []string
	if p := offeringPrice(o); p != "" {
		lines = append(lines, "Price: "+p)
	}
	if d := strings.TrimSpace(str(o["description"])); d != "" {
		lines = append(lines, "About it: "+excerpt(d, 800))
	}
	if c := strings.TrimSpace(str(o["promo_code"])); c != "" {
		lines = append(lines, "Promotion code: "+c)
	}
	if e := strings.TrimSpace(str(o["ends_at"])); e != "" {
		if t, err := time.Parse(time.RFC3339, e); err == nil {
			e = t.Format("2 January 2006")
		}
		lines = append(lines, "Offer ends: "+e)
	}
	return strings.Join(lines, "\n")
}

// offeringPrice is the offering's price as shown, e.g. "KES 850".
func offeringPrice(o map[string]any) string {
	switch p := o["price"].(type) {
	case float64:
		if p > 0 {
			return fmt.Sprintf("KES %s", formatAmount(p))
		}
	case string:
		return strings.TrimSpace(p)
	}
	return ""
}

// formatAmount writes a price with thousands separators and no needless decimals.
func formatAmount(p float64) string {
	whole := int64(p)
	s := fmt.Sprintf("%d", whole)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if frac := p - float64(whole); frac > 0.004 {
		s += fmt.Sprintf(".%02d", int64(frac*100+0.5))
	}
	return s
}

// scriptText is every word a video ad shows or says, for the restricted-category flags.
func scriptText(copyFields map[string]any) []string {
	var out []string
	var script VideoScript
	if b, err := json.Marshal(copyFields["script"]); err == nil && json.Unmarshal(b, &script) == nil {
		out = append(out, script.Hooks...)
		for _, sc := range script.Scenes {
			out = append(out, sc.Voiceover, sc.Caption)
		}
	}
	if card, ok := copyFields["end_card"].(map[string]any); ok {
		for _, k := range []string{"headline", "subline", "cta"} {
			out = append(out, str(card[k]))
		}
	}
	return out
}

// toMap turns a struct into the JSON map ent stores.
func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
