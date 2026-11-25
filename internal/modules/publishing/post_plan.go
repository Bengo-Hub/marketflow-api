package publishing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
	"github.com/bengobox/marketflow-api/internal/modules/publishing/postingplan"
	"github.com/bengobox/marketflow-api/internal/platform/postcaption"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// The post plan (owner, 2026-11-25): what a finished video is posted with, from the business's
// settings. The Studio's Post dialog starts with it, and the WhatsApp message shows it and posts
// exactly it (its fingerprint), so the web and WhatsApp follow one rule.

// Best time default: the platform's service_config row, the tenant's metadata over it. "on" unless
// either says off.
const (
	BestTimeConfigKey = "publishing.post_at_best_time"
	BestTimeTenantKey = "post_at_best_time"
)

// bestTimeOn reports whether posts start with each platform's best time.
func bestTimeOn(ctx context.Context, db *ent.Client, t *ent.Tenant) bool {
	if t != nil {
		if s, _ := t.Metadata[BestTimeTenantKey].(string); s != "" {
			return !strings.EqualFold(strings.TrimSpace(s), "off")
		}
	}
	return !strings.EqualFold(svcconfig.Value(ctx, db, uuid.Nil, BestTimeConfigKey), "off")
}

// PlannedPost is one account a post goes to and when. At is zero for "now".
type PlannedPost struct {
	AccountID   string     `json:"connected_account_id"`
	Platform    string     `json:"platform"`
	DisplayName string     `json:"display_name"`
	At          *time.Time `json:"at,omitempty"`
	// Note says what the platform will do differently (TikTok's privacy).
	Note string `json:"note,omitempty"`
}

// PostPlan is the whole post.
type PostPlan struct {
	VersionID   string           `json:"version_id"`
	Caption     string           `json:"caption"`
	AIGenerated bool             `json:"ai_generated"`
	BestTime    bool             `json:"best_time"`
	Timezone    string           `json:"timezone"`
	Posts       []PlannedPost    `json:"posts"`
	Skipped     []SkippedAccount `json:"skipped"`
	Fingerprint string           `json:"fingerprint"`

	targets []target
	asset   *ent.CreativeAsset
	version *ent.CreativeVersion
	loc     *time.Location
}

// planRefusal is why there is nothing to post, as the WhatsApp route answers it.
type planRefusal struct{ code, msg string }

func (e *planRefusal) Error() string { return e.msg }

// tiktokNote is said about a TikTok post made without the Studio's choices: TikTok's guidelines
// ask a person to choose who sees it, so it starts private.
const tiktokNote = "posted as private; choose who can see it in the TikTok app, or post it from the Studio to choose first"

// BuildPostPlan plans a finished video's post: every connected account that can take it, the
// caption, the AI label and each platform's best time.
func (h *Handler) BuildPostPlan(ctx context.Context, tid, vid uuid.UUID, now time.Time) (*PostPlan, error) {
	v, err := h.Store.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid)).Only(ctx)
	if err != nil {
		return nil, err
	}
	asset, err := h.Store.DB.CreativeAsset.Query().
		Where(creativeasset.VersionIDEQ(vid), creativeasset.TenantIDEQ(tid), creativeasset.KindEQ("video"), creativeasset.PurgedAtIsNil()).
		First(ctx)
	if ent.IsNotFound(err) {
		return nil, &planRefusal{"no_video", "the video file is not available any more"}
	}
	if err != nil {
		return nil, err
	}
	types := make([]string, 0, len(accountPlatforms))
	for t := range accountPlatforms {
		types = append(types, t)
	}
	accounts, err := h.Store.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.StatusEQ("active"), connectedaccount.AccountTypeIn(types...)).
		Order(ent.Asc(connectedaccount.FieldAccountType), ent.Asc(connectedaccount.FieldDisplayName)).
		Limit(50).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, &planRefusal{"no_accounts", "no connected account can take an organic post"}
	}
	t, _ := h.Store.DB.Tenant.Get(ctx, tid)
	tz, loc := postingplan.Location(t)
	p := &PostPlan{VersionID: vid.String(), AIGenerated: aiLabelOn(ctx, h.Store.DB, tid), BestTime: bestTimeOn(ctx, h.Store.DB, t), Timezone: tz,
		Caption: postcaption.Compose(postcaption.Parts{Words: postcaption.Words(v.Copy), PPB: postcaption.PPBLine(v.Metadata)}),
		Posts:   []PlannedPost{}, Skipped: []SkippedAccount{}, asset: asset, version: v, loc: loc}
	if h.Caption != nil {
		if c, err := h.Caption(ctx, tid, vid); err == nil {
			p.Caption = c
		}
	}
	fps, _ := asset.Metadata["fps"].(float64)
	video := Video{URL: asset.URL, Bytes: asset.Bytes, DurationMS: asset.DurationMs, Width: asset.Width, Height: asset.Height, FPS: fps}
	targets, skipped := pickTargets(accounts, func(pl string) bool { return h.Store.Enabled(ctx, pl) }, video, Post{Caption: p.Caption, AIGenerated: p.AIGenerated})
	if skipped != nil {
		p.Skipped = skipped
	}
	var times *postingplan.Settings
	if p.BestTime {
		s := postingplan.Load(ctx, h.Store.DB, tid)
		times = &s
	}
	p.targets = targets
	for _, tg := range targets {
		post := PlannedPost{AccountID: tg.acc.ID.String(), Platform: tg.platform, DisplayName: tg.acc.DisplayName}
		if times != nil {
			// A window that is open now posts now.
			if at := times.BestTime(tg.platform, now).UTC().Truncate(time.Second); at.After(now.Add(time.Minute)) {
				post.At = &at
			}
		}
		if tg.platform == TikTok {
			post.Note = tiktokNote
		}
		p.Posts = append(p.Posts, post)
	}
	p.Fingerprint = postFingerprint(p)
	return p, nil
}

// postFingerprint binds an approval to the post it showed: the video file, the caption, the label
// and each account with its time.
func postFingerprint(p *PostPlan) string {
	parts := []string{p.VersionID, p.asset.ID.String(), p.Caption, fmt.Sprint(p.AIGenerated)}
	list := make([]string, 0, len(p.Posts))
	for _, x := range p.Posts {
		at := "now"
		if x.At != nil {
			at = x.At.Format(time.RFC3339)
		}
		list = append(list, x.AccountID+"@"+at)
	}
	sort.Strings(list)
	sum := sha256.Sum256([]byte(strings.Join(append(parts, list...), "\x00")))
	return hex.EncodeToString(sum[:])[:32]
}

// nothing is the refusal of a plan with no account to post to.
func (p *PostPlan) nothing() *planRefusal {
	if len(p.Posts) > 0 {
		return nil
	}
	reasons := make([]string, 0, len(p.Skipped))
	for _, s := range p.Skipped {
		reasons = append(reasons, s.DisplayName+": "+s.Reason)
	}
	return &planRefusal{"nothing_postable", "no account can take it now (" + strings.Join(reasons, "; ") + ")"}
}

// whatsAppPlan is the plan WhatsApp may post: an ad held for a person's review, or one no account
// can take, is refused.
func (h *Handler) whatsAppPlan(ctx context.Context, tid, vid uuid.UUID, now time.Time) (*PostPlan, error) {
	p, err := h.BuildPostPlan(ctx, tid, vid, now)
	if err != nil {
		return nil, err
	}
	if err := brandimages.CheckUse(ctx, h.Store.DB, tid, vid); errors.Is(err, brandimages.ErrFaceUse) {
		return nil, &planRefusal{"consent_ended", err.Error()}
	} else if err != nil {
		return nil, err
	}
	if h.WhatsApp.NeedsReview != nil {
		if reasons := h.WhatsApp.NeedsReview(p.version); len(reasons) > 0 {
			return nil, &planRefusal{"needs_review", strings.Join(reasons, "; ")}
		}
	}
	if n := p.nothing(); n != nil {
		return nil, n
	}
	return p, nil
}

// PostPlanText is the plan for a WhatsApp message, without the caption (shown with the video).
func PostPlanText(p *PostPlan) string {
	var b strings.Builder
	b.WriteString("Post to:")
	for _, x := range p.Posts {
		when := "now"
		if x.At != nil {
			when = x.At.In(p.loc).Format("Mon 2 Jan 15:04")
		}
		fmt.Fprintf(&b, "\n- %s (%s), %s", x.DisplayName, title(x.Platform), when)
		if x.Note != "" {
			b.WriteString(", " + x.Note)
		}
	}
	for _, s := range p.Skipped {
		fmt.Fprintf(&b, "\n- Not %s: %s", s.DisplayName, s.Reason)
	}
	if p.AIGenerated {
		b.WriteString("\nMade with AI label: on")
	} else {
		b.WriteString("\nMade with AI label: off")
	}
	return b.String()
}

func (h *Handler) planFailed(w http.ResponseWriter, err error) {
	var pr *planRefusal
	if errors.As(err, &pr) {
		httpapi.Error(w, http.StatusConflict, pr.code, pr.msg, nil)
		return
	}
	h.fail(w, err)
}

// PostPlanFor handles GET /api/v1/publishing/post-plan?version_id=: what the Post dialog starts
// with, the same plan WhatsApp shows and posts.
// @Summary What a finished video is posted with
// @Tags Publishing
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param version_id query string true "Version UUID"
// @Success 200 {object} PostPlan
// @Security bearerAuth
// @Router /publishing/post-plan [get]
func (h *Handler) PostPlanFor(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	vid, err := uuid.Parse(r.URL.Query().Get("version_id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "version_id is required", nil)
		return
	}
	p, err := h.BuildPostPlan(r.Context(), tid, vid, time.Now().UTC())
	if err != nil {
		h.planFailed(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, p)
}

// OSTurnPostPlan handles the S2S POST /api/v1/internal/creative/os-turn/post-plan: the plan and
// its message for the WhatsApp approval. 200 {plan, text, caption, fingerprint}; the same refusals
// as the publish route. Nothing is approved or posted.
func (h *Handler) OSTurnPostPlan(w http.ResponseWriter, r *http.Request) {
	_, tid, vid, _, ok := h.readOSTurn(w, r)
	if !ok {
		return
	}
	p, err := h.whatsAppPlan(r.Context(), tid, vid, time.Now().UTC())
	if err != nil {
		h.planFailed(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"plan": p, "text": PostPlanText(p), "caption": p.Caption, "fingerprint": p.Fingerprint})
}
