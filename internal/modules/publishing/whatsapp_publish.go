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
	// Fingerprint is the post plan the approval's message showed (post_plan.go).
	Fingerprint string `json:"fingerprint,omitempty"`
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
// key): approve the version and post it as the plan the message showed (post_plan.go). 200
// {posted, skipped}; 403 when the user may not post; 409 plan_changed (with the plan as it is now),
// needs_review, no_video, no_accounts or nothing_postable, with the reason in error.
func (h *Handler) OSTurnPublish(w http.ResponseWriter, r *http.Request) {
	in, tid, vid, uid, ok := h.readOSTurn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	plan, err := h.whatsAppPlan(ctx, tid, vid, now)
	if err != nil {
		h.planFailed(w, err)
		return
	}
	// The approval covers what its message showed; anything else asks again. Fingerprint is empty
	// only for a code issued before plans were shown.
	if in.Fingerprint != "" && in.Fingerprint != plan.Fingerprint {
		httpapi.Error(w, http.StatusConflict, "plan_changed", "the post changed since it was shown", map[string]any{
			"text": PostPlanText(plan), "caption": plan.Caption, "fingerprint": plan.Fingerprint})
		return
	}
	// Approve only once something can be posted, so a refusal above leaves the draft as it was.
	if plan.version.Status != "approved" {
		if err := h.WhatsApp.Approve(ctx, tid, vid, uid); err != nil {
			h.approveFailed(w, err)
			return
		}
	}
	ref := in.RunID + ":" + vid.String()
	posted := make([]PostedAccount, 0, len(plan.targets))
	for i, t := range plan.targets {
		key := plan.asset.ID.String() + ":" + t.acc.ID.String()
		at := now
		if x := plan.Posts[i].At; x != nil {
			at = *x
		}
		meta := map[string]any{"ai_generated": plan.AIGenerated, "made_for_kids": false, "planned_at": at.Format(time.RFC3339),
			"requested_via": "whatsapp"}
		if plan.BestTime {
			meta["best_time"] = true
		}
		if strings.TrimSpace(in.Sender) != "" {
			meta[MetaWhatsApp] = map[string]any{"to": strings.TrimSpace(in.Sender), "ref": ref, "user_id": uid.String()}
		}
		row, err := h.Store.DB.PublishJob.Create().
			SetTenantID(tid).SetAssetID(plan.asset.ID).SetConnectedAccountID(t.acc.ID).SetPlatform(t.platform).
			SetCaption(plan.Caption).SetScheduledAt(at).SetDedupeKey(key).SetMetadata(meta).SetCreatedBy(uid).
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
			zap.Int("accounts", len(posted)), zap.Int("skipped", len(plan.Skipped)))
	}
	if h.Kick != nil {
		h.Kick()
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"posted": posted, "skipped": plan.Skipped})
}

// readOSTurn reads an internal WhatsApp route's body and checks the person may post, writing the
// refusal. The role is checked on every call: it may have been removed since the message was sent.
func (h *Handler) readOSTurn(w http.ResponseWriter, r *http.Request) (in whatsAppPublishInput, tid, vid, uid uuid.UUID, ok bool) {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return in, tid, vid, uid, false
	}
	var err1, err2, err3 error
	tid, err1 = uuid.Parse(in.TenantID)
	vid, err2 = uuid.Parse(in.VersionID)
	uid, err3 = uuid.Parse(in.UserID)
	if err1 != nil || err2 != nil || err3 != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "tenant_id, version_id and user_id are required", nil)
		return in, tid, vid, uid, false
	}
	if h.WhatsApp.IsApprover == nil || h.WhatsApp.Approve == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "unavailable", "posting from WhatsApp is not configured", nil)
		return in, tid, vid, uid, false
	}
	if !h.WhatsApp.IsApprover(r.Context(), tid, uid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can post", nil)
		return in, tid, vid, uid, false
	}
	return in, tid, vid, uid, true
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
