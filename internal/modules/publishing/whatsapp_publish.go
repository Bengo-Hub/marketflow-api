package publishing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Approve and post from WhatsApp (Sprint 17). The owner taps Approve and post (or types POST
// <code>) under the finished ad; marketflow-ai checks the code (single use, bound to the run, the
// user and the number) and calls this internal route. It does what the Studio's Approve and the
// publish dialog do, with the same checks: the approving roles, the compliance and
// restricted-category holds, the PPB hold, active connected accounts, each platform's switch and
// the preflight. It is organic posting only: paid ads are never started from WhatsApp.

// MetaWhatsApp is the publish job metadata key that names the WhatsApp requester, so the result
// of the posts can be sent back to them (worker.WhatsAppPublishResult).
const MetaWhatsApp = "whatsapp"

// WhatsAppPublishDeps are the creative and role checks the route needs; app.go wires them so this
// package does not import creative.
type WhatsAppPublishDeps struct {
	// IsApprover reports whether the auth user is an active MarketFlow user of the tenant with an
	// approving role (owner or MarketFlow admin), read from MarketFlow's own roles.
	IsApprover func(ctx context.Context, tenantID, authUserID uuid.UUID) bool
	// NeedsReview lists why a version must be looked at by a person before it is posted
	// (creative.FlaggedForReview); empty means nothing was flagged.
	NeedsReview func(v *ent.CreativeVersion) []string
	// Approve approves a version waiting for review on the user's behalf (channel whatsapp). A
	// version already approved is left as it is.
	Approve func(ctx context.Context, tenantID, versionID, by uuid.UUID) error
}

// whatsAppPublishInput is the internal route's body.
type whatsAppPublishInput struct {
	TenantID  string `json:"tenant_id"`
	VersionID string `json:"version_id"`
	UserID    string `json:"user_id"`
	RunID     string `json:"run_id"`
	// Sender is the verified number the approval came from; the post results go back to it.
	Sender string `json:"sender"`
}

// PostedAccount and SkippedAccount are what the route reports back for the WhatsApp reply.
type PostedAccount struct {
	Platform    string `json:"platform"`
	DisplayName string `json:"display_name"`
	JobID       string `json:"job_id"`
}

type SkippedAccount struct {
	DisplayName string `json:"display_name"`
	Reason      string `json:"reason"`
}

// target is one account a post will go to.
type target struct {
	acc      *ent.ConnectedAccount
	platform string
}

// pickTargets splits the tenant's accounts into those the video can go to now and those it
// cannot, with the reason, exactly as the publish dialog would refuse them.
func pickTargets(accounts []*ent.ConnectedAccount, enabled func(platform string) bool, video Video, post Post) ([]target, []SkippedAccount) {
	var out []target
	var skipped []SkippedAccount
	on := map[string]bool{}
	for _, acc := range accounts {
		platform := PlatformForAccountType(acc.AccountType)
		if platform == "" {
			continue // ad accounts and others never take an organic post
		}
		ok, seen := on[platform]
		if !seen {
			ok = enabled(platform)
			on[platform] = ok
		}
		switch {
		case !ok:
			skipped = append(skipped, SkippedAccount{DisplayName: acc.DisplayName, Reason: fmt.Sprintf("posting to %s is not switched on yet", title(platform))})
		case len(MissingScopes(platform, acc.Scopes)) > 0:
			skipped = append(skipped, SkippedAccount{DisplayName: acc.DisplayName, Reason: "it needs reconnecting in MarketFlow"})
		default:
			if problems := Preflight(platform, video, post, acc.Scopes, 0); len(problems) > 0 {
				skipped = append(skipped, SkippedAccount{DisplayName: acc.DisplayName, Reason: strings.Join(problems, "; ")})
				continue
			}
			out = append(out, target{acc: acc, platform: platform})
		}
		if len(out) >= maxAccountsPerPost {
			break
		}
	}
	return out, skipped
}

// OSTurnPublish handles the S2S POST /api/v1/internal/creative/os-turn/publish (internal service
// key): approve the version and post it now to every connected account that can take it. 200
// {posted, skipped}; 403 when the user may not post; 409 needs_review, ppb_approval_required,
// no_video, no_accounts or nothing_postable, with the reason in error.
func (h *Handler) OSTurnPublish(w http.ResponseWriter, r *http.Request) {
	var in whatsAppPublishInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	tid, err1 := uuid.Parse(in.TenantID)
	vid, err2 := uuid.Parse(in.VersionID)
	uid, err3 := uuid.Parse(in.UserID)
	if err1 != nil || err2 != nil || err3 != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "tenant_id, version_id and user_id are required", nil)
		return
	}
	d := h.WhatsApp
	if d.IsApprover == nil || d.Approve == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "unavailable", "posting from WhatsApp is not configured", nil)
		return
	}
	ctx := r.Context()
	// The role is checked again now: it may have been removed since the message was sent.
	if !d.IsApprover(ctx, tid, uid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can post", nil)
		return
	}
	v, err := h.Store.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid)).Only(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	if d.NeedsReview != nil {
		if reasons := d.NeedsReview(v); len(reasons) > 0 {
			httpapi.Error(w, http.StatusConflict, "needs_review", strings.Join(reasons, "; "), map[string]any{"reasons": reasons})
			return
		}
	}
	asset, err := h.Store.DB.CreativeAsset.Query().
		Where(creativeasset.VersionIDEQ(vid), creativeasset.TenantIDEQ(tid), creativeasset.KindEQ("video"), creativeasset.PurgedAtIsNil()).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			httpapi.Error(w, http.StatusConflict, "no_video", "the video file is not available any more", nil)
			return
		}
		h.fail(w, err)
		return
	}
	types := make([]string, 0, len(accountPlatforms))
	for t := range accountPlatforms {
		types = append(types, t)
	}
	accounts, err := h.Store.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.StatusEQ("active"), connectedaccount.AccountTypeIn(types...)).
		Order(ent.Asc(connectedaccount.FieldAccountType), ent.Asc(connectedaccount.FieldDisplayName)).
		Limit(50).
		All(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	if len(accounts) == 0 {
		httpapi.Error(w, http.StatusConflict, "no_accounts", "no connected account can take an organic post", nil)
		return
	}
	fps, _ := asset.Metadata["fps"].(float64)
	video := Video{URL: asset.URL, Bytes: asset.Bytes, DurationMS: asset.DurationMs, Width: asset.Width, Height: asset.Height, FPS: fps}
	post := Post{Caption: whatsAppCaption(v), AIGenerated: aiLabelOn(ctx, h.Store.DB, tid)}
	targets, skipped := pickTargets(accounts, func(p string) bool { return h.Store.Enabled(ctx, p) }, video, post)
	if len(targets) == 0 {
		reasons := make([]string, 0, len(skipped))
		for _, s := range skipped {
			reasons = append(reasons, s.DisplayName+": "+s.Reason)
		}
		httpapi.Error(w, http.StatusConflict, "nothing_postable", "no account can take it now ("+strings.Join(reasons, "; ")+")", nil)
		return
	}
	// Approve only once something can be posted, so a refusal above leaves the draft as it was.
	if v.Status != "approved" {
		if err := d.Approve(ctx, tid, vid, uid); err != nil {
			h.approveFailed(w, err)
			return
		}
	}
	now := time.Now().UTC()
	ref := in.RunID + ":" + vid.String()
	posted := make([]PostedAccount, 0, len(targets))
	for _, t := range targets {
		key := asset.ID.String() + ":" + t.acc.ID.String()
		meta := map[string]any{"ai_generated": post.AIGenerated, "made_for_kids": false, "planned_at": now.Format(time.RFC3339),
			"requested_via": "whatsapp"}
		if strings.TrimSpace(in.Sender) != "" {
			meta[MetaWhatsApp] = map[string]any{"to": strings.TrimSpace(in.Sender), "ref": ref, "user_id": uid.String()}
		}
		row, err := h.Store.DB.PublishJob.Create().
			SetTenantID(tid).SetAssetID(asset.ID).SetConnectedAccountID(t.acc.ID).SetPlatform(t.platform).
			SetCaption(post.Caption).SetScheduledAt(now).SetDedupeKey(key).SetMetadata(meta).SetCreatedBy(uid).
			Save(ctx)
		if ent.IsConstraintError(err) {
			// Already scheduled from the Studio or an earlier tap: one post per account, never two.
			row, err = h.Store.DB.PublishJob.Query().Where(publishjob.DedupeKeyEQ(key), publishjob.TenantIDEQ(tid)).Only(ctx)
		}
		if err != nil {
			h.fail(w, err)
			return
		}
		posted = append(posted, PostedAccount{Platform: t.platform, DisplayName: t.acc.DisplayName, JobID: row.ID.String()})
	}
	if h.Logger != nil {
		h.Logger.Info("whatsapp approve and post", zap.String("tenant_id", tid.String()), zap.String("version_id", vid.String()),
			zap.Int("accounts", len(posted)), zap.Int("skipped", len(skipped)))
	}
	if h.Kick != nil {
		h.Kick()
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"posted": posted, "skipped": skipped})
}

// ErrPPBHold is how app.go reports creative.ErrPPBApprovalRequired to this package.
var ErrPPBHold = errors.New("a PPB approval reference is required before this ad is approved")

func (h *Handler) approveFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrPPBHold) {
		httpapi.Error(w, http.StatusConflict, "needs_review", "a pharmacy medicine ad needs its PPB approval reference entered first", nil)
		return
	}
	if ent.IsNotFound(err) {
		h.fail(w, err)
		return
	}
	httpapi.Error(w, http.StatusConflict, "not_approvable", "the video can no longer be approved from WhatsApp: "+err.Error(), nil)
}

// whatsAppCaption is the post's caption: the copy the Studio made for the video, if any.
func whatsAppCaption(v *ent.CreativeVersion) string {
	for _, k := range []string{"caption", "primary_text", "body"} {
		if s, ok := v.Copy[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
