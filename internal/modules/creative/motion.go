package creative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A motion poster is a short clip made from a finished poster's scene, with the
// poster's words laid over it by marketflow-ai. A clip takes minutes, so it runs
// as a job there: the renderer starts it, and marketflow-ai calls back
// (POST /api/v1/internal/creative/clips/{version_id}) when it is done or failed.
// Polling the job is only the fallback for a callback that never arrived.

const (
	minClipSeconds = 4
	maxClipSeconds = 10
	// clipPollAfter is how long a clip may go without news before the renderer
	// asks marketflow-ai for it (the callback normally comes first).
	clipPollAfter = 2 * time.Minute
	// clipDeadline fails a clip still unsettled this long after it first started.
	clipDeadline  = 45 * time.Minute
	clipPollBatch = 20
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
	if s.quota != nil {
		if res, _ := s.quota.Check(ctx, tenantID.String(), "creative_renders", dailyRenderCap); res != nil && !res.Allowed {
			return nil, ErrDailyCap
		}
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

// ClipJob is marketflow-ai's clip job state: the start reply, the callback body
// and the status poll all carry it.
type ClipJob struct {
	Status string `json:"status"` // running | done | failed
	Clip   *struct {
		URL      string  `json:"url"`
		Key      string  `json:"key"`
		Bytes    int64   `json:"bytes"`
		Seconds  int     `json:"seconds"`
		Width    int     `json:"width"`
		Height   int     `json:"height"`
		SHA256   string  `json:"sha256"`
		Route    string  `json:"route"`
		CostUSD  float64 `json:"cost_usd"`
		Attempts int     `json:"attempts"`
	} `json:"clip,omitempty"`
	CreditsCharged int    `json:"credits_charged"`
	Error          string `json:"error,omitempty"`
	// Refunded is false on a failed job that was lost with its pod: its charge
	// stands, so it is started again under the same charge key.
	Refunded bool `json:"refunded"`
}

// errClipBusy is a 429: marketflow-ai is making as many clips as it may.
var errClipBusy = errors.New("marketflow-ai is busy with other clips")

// startClip asks marketflow-ai to make a claimed video version. It returns at
// once; the clip settles through the callback (or the fallback poll).
func (r *Renderer) startClip(ctx context.Context, v *ent.CreativeVersion) {
	meta := copyMap(v.Metadata)
	meta["attempts"] = intOf(meta["attempts"]) + 1
	if _, ok := meta["clip_started_at"]; !ok {
		meta["clip_started_at"] = r.nowFn().UTC().Format(time.RFC3339)
	}
	_, _ = r.db.CreativeVersion.UpdateOneID(v.ID).SetMetadata(meta).Save(ctx)

	call := clipCall{TenantID: v.TenantID.String(), VersionID: v.ID.String(), SceneURL: str(meta["scene_url"]),
		Prompt: str(meta["motion_prompt"]), Format: v.Format, Seconds: intOf(meta["seconds"]), Tier: v.Tier,
		Headline: str(v.Copy["headline"]), Subline: str(v.Copy["subline"]), Price: str(v.Copy["price"]),
		CTA: str(v.Copy["cta"]), Charge: true}
	if b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx); err == nil {
		brand, _ := b.Metadata["brand"].(map[string]any)
		call.Accent, call.LogoURL = str(brand["accent"]), str(brand["logo_url"])
	}
	job, err := r.clipRequest(ctx, http.MethodPost, "/api/v1/internal/creative/clip", call)
	switch {
	case errors.Is(err, errClipBusy):
		// Not an attempt: back in the queue for the next tick.
		meta["attempts"] = intOf(meta["attempts"]) - 1
		_ = r.db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusQueued).SetMetadata(meta).Exec(ctx)
	case err != nil:
		r.failClip(ctx, v.ID, err)
	case job.Status != "running":
		// Already done (a restart after the clip was made): settle it now.
		if err := r.SettleClip(ctx, v.ID, *job); err != nil {
			r.logger.Error("clip made but not recorded", zap.String("version_id", v.ID.String()), zap.Error(err))
		}
	}
}

// pollClips is the fallback for a lost callback: it asks marketflow-ai about
// clips that have had no news for clipPollAfter, and fails those past the deadline.
func (r *Renderer) pollClips(ctx context.Context) {
	waiting, err := r.db.CreativeVersion.Query().
		Where(creativeversion.StatusEQ(StatusRendering), creativeversion.KindEQ(KindVideo),
			creativeversion.UpdatedAtLT(r.nowFn().Add(-clipPollAfter))).
		Limit(clipPollBatch).
		All(ctx)
	if err != nil {
		return
	}
	for _, v := range waiting {
		job, err := r.clipRequest(ctx, http.MethodGet, "/api/v1/internal/creative/clip/"+v.ID.String(), nil)
		if err == nil {
			if err := r.SettleClip(ctx, v.ID, *job); err != nil {
				r.logger.Warn("clip poll not recorded", zap.String("version_id", v.ID.String()), zap.Error(err))
			}
		}
		if err != nil || job.Status == "running" {
			r.touchClip(ctx, v, errors.Is(err, errClipLost))
		}
	}
}

// touchClip records a poll with no result so the next one waits clipPollAfter;
// a clip past its deadline fails, and one marketflow-ai has no job for goes
// back in the queue.
func (r *Renderer) touchClip(ctx context.Context, v *ent.CreativeVersion, lost bool) {
	started, _ := time.Parse(time.RFC3339, str(v.Metadata["clip_started_at"]))
	u := r.db.CreativeVersion.Update().
		Where(creativeversion.ID(v.ID), creativeversion.StatusEQ(StatusRendering), creativeversion.UpdatedAt(v.UpdatedAt))
	meta := copyMap(v.Metadata)
	switch {
	case !started.IsZero() && r.nowFn().Sub(started) > clipDeadline, lost && intOf(meta["attempts"]) >= maxAttempts:
		u.SetStatus(StatusFailed).SetError("the video took too long; try again")
	case lost:
		u.SetStatus(StatusQueued)
	default:
		meta["polled_at"] = r.nowFn().UTC().Format(time.RFC3339)
		u.SetMetadata(meta)
	}
	_, _ = u.Save(ctx)
}

// SettleClip records a clip job's outcome on its video version, from the
// callback or the poll. It is idempotent: only a version still rendering
// changes, so a late or repeated report does nothing.
func (r *Renderer) SettleClip(ctx context.Context, id uuid.UUID, job ClipJob) error {
	v, err := r.db.CreativeVersion.Query().
		Where(creativeversion.ID(id), creativeversion.KindEQ(KindVideo)).
		Only(ctx)
	if err != nil {
		return err
	}
	if v.Status != StatusRendering {
		return nil
	}
	pending := r.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.StatusEQ(StatusRendering))
	switch {
	case job.Status == "done" && job.Clip != nil && job.Clip.URL != "" && job.Clip.Key != "":
		c := job.Clip
		meta := copyMap(v.Metadata)
		meta["route"], meta["cost_usd"], meta["provider_attempts"] = c.Route, c.CostUSD, c.Attempts
		return withTx(ctx, r.db, func(tx *ent.Tx) error {
			n, err := tx.CreativeVersion.Update().
				Where(creativeversion.ID(id), creativeversion.StatusEQ(StatusRendering)).
				SetStatus(StatusReady).SetCredits(job.CreditsCharged).SetMetadata(meta).ClearError().
				Save(ctx)
			if err != nil || n == 0 {
				return err
			}
			a := tx.CreativeAsset.Create().
				SetTenantID(v.TenantID).SetVersionID(id).SetKind(AssetVideo).
				SetStorageKey(c.Key).SetURL(c.URL).SetBytes(c.Bytes).
				SetWidth(c.Width).SetHeight(c.Height).SetDurationMs(c.Seconds * 1000).
				SetExpiresAt(r.nowFn().Add(DraftTTL))
			if c.SHA256 != "" {
				a.SetSha256(c.SHA256)
			}
			_, err = a.Save(ctx)
			return err
		})
	case job.Status == "failed" && job.Refunded:
		msg := job.Error
		if msg == "" {
			msg = "the video could not be made right now; nothing was charged"
		}
		_, err := pending.SetStatus(StatusFailed).SetError(excerpt(msg, 480)).Save(ctx)
		return err
	case job.Status == "failed":
		// Lost with its pod and not refunded: start again under the same charge key.
		if intOf(v.Metadata["attempts"]) >= maxAttempts {
			_, err := pending.SetStatus(StatusFailed).SetError("the video took too long; try again").Save(ctx)
			return err
		}
		_, err := pending.SetStatus(StatusQueued).Save(ctx)
		return err
	}
	return nil // still running
}

// errClipLost is a 404 from the status poll: marketflow-ai has no job for it.
var errClipLost = errors.New("marketflow-ai has no job for this clip")

func (r *Renderer) clipRequest(ctx context.Context, method, path string, body any) (*ClipJob, error) {
	var rd io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rd = bytes.NewReader(buf)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, r.aiBaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpware.HeaderAPIKey, r.serviceKey)
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted:
		var job ClipJob
		if err := json.Unmarshal(data, &job); err != nil || job.Status == "" {
			return nil, fmt.Errorf("marketflow-ai sent an unreadable clip reply")
		}
		return &job, nil
	case http.StatusPaymentRequired:
		return nil, errNoCredits
	case http.StatusTooManyRequests:
		return nil, errClipBusy
	case http.StatusNotFound:
		if method == http.MethodGet {
			return nil, errClipLost
		}
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	if e.Error == "" {
		e.Error = http.StatusText(resp.StatusCode)
	}
	return nil, fmt.Errorf("marketflow-ai %d: %s", resp.StatusCode, e.Error)
}

// failClip records why a clip did not start. marketflow-ai charges only once a
// job has started, so nothing was charged.
func (r *Renderer) failClip(ctx context.Context, id uuid.UUID, err error) {
	msg := "the video could not be made right now; nothing was charged"
	if errors.Is(err, errNoCredits) {
		msg = "not enough AI credits for this video"
	}
	r.logger.Warn("clip did not start", zap.String("version_id", id.String()), zap.Error(err))
	_ = r.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.StatusEQ(StatusRendering)).
		SetStatus(StatusFailed).SetError(msg).Exec(ctx)
}
