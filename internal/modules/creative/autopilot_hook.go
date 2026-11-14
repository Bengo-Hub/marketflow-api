package creative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
)

// Ad autopilot videos in veto-window mode (Sprint 20, owner 2026-11-14). An ad_autopilot agent
// whose approval mode is auto_publish carries its draft on by itself: script, storyboard, the
// storyboard approved by the agent, the render, then an organic post scheduled after the veto
// window. Each step goes through the same Studio routes, charges, refunds and caps as a person's.
// The worker's autopublish pipeline does the steps; the Renderer only tells it a step settled,
// the same way os_turn.go tells a WhatsApp requester. The version carries metadata.autopilot
// (MetaAutopilot) from the run that made it; a version without it is never touched.

// MetaAutopilot is the version metadata key of an autopilot video's veto-window state.
const MetaAutopilot = "autopilot"

// Autopilot step events the Renderer reports.
const (
	AutopilotScriptReady     = "script_ready"
	AutopilotStoryboardReady = "storyboard_ready"
	AutopilotStoryboardError = "storyboard_failed"
	AutopilotRenderDone      = "render_done"
	AutopilotRenderFailed    = "render_failed"
)

// autopilotNotifyTimeout bounds one step the pipeline takes after a report.
const autopilotNotifyTimeout = 60 * time.Second

// AutopilotHook advances an autopilot video after one of its steps settled. It must be safe to
// call twice for the same step: the pipeline reads the version's current state each time.
type AutopilotHook interface {
	Settled(ctx context.Context, tenantID, versionID uuid.UUID, event string)
}

// WithAutopilot turns on the veto-window steps of autopilot videos.
func (r *Renderer) WithAutopilot(h AutopilotHook) *Renderer {
	r.autopilot = h
	return r
}

// IsAutopilotVersion reports whether a version was made by an autopilot run in veto-window mode.
func IsAutopilotVersion(v *ent.CreativeVersion) bool {
	ap, ok := v.Metadata[MetaAutopilot].(map[string]any)
	return ok && str(ap["agent_id"]) != ""
}

// IsPresenterVersion reports whether a video ad has a presenter (presenter or presenter_premium):
// those keep the storyboard step; a template ad goes from its script straight to the render.
func IsPresenterVersion(v *ent.CreativeVersion) bool {
	return isPresenter(videoKind(v))
}

// notifyAutopilot hands a settled step to the pipeline in the background, so a job callback never
// waits on it. Only the report that settled the version calls it.
func (r *Renderer) notifyAutopilot(ctx context.Context, v *ent.CreativeVersion, event string) {
	if r.autopilot == nil || v == nil || !IsAutopilotVersion(v) {
		return
	}
	tenantID, id := v.TenantID, v.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autopilotNotifyTimeout)
		defer cancel()
		r.autopilot.Settled(ctx, tenantID, id, event)
	}()
}

// notifyAutopilotByID is notifyAutopilot for a caller that holds only the version id.
func (r *Renderer) notifyAutopilotByID(ctx context.Context, id uuid.UUID, event string) {
	if r.autopilot == nil {
		return
	}
	if v, err := r.db.CreativeVersion.Get(ctx, id); err == nil {
		r.notifyAutopilot(ctx, v, event)
	}
}

// QuoteRender is what rendering a video ad would cost now against the tenant's balance, read from
// marketflow-ai's quote (the same quote the Studio shows before a render). Nothing is charged.
type QuoteRender struct {
	CreditsNeeded int  `json:"credits_needed"`
	Balance       int  `json:"balance"` // -1: unlimited or unknown
	Enough        bool `json:"enough"`
}

// QuoteRenderCredits reads a video ad's render quote.
func (r *Renderer) QuoteRenderCredits(ctx context.Context, v *ent.CreativeVersion) (QuoteRender, error) {
	var q QuoteRender
	data, status, err := r.QuoteVideoAd(ctx, v)
	if err != nil {
		return q, err
	}
	if status != http.StatusOK {
		return q, aiStatusError(status, data)
	}
	if err := json.Unmarshal(data, &q); err != nil {
		return q, errors.New("marketflow-ai sent an unreadable quote")
	}
	return q, nil
}

// ApproveAfterVeto approves an autopilot video whose veto window passed without a veto. The
// evidence channel is veto_window_elapsed and no user is recorded, so it never reads as a
// person's approval. A version a person already approved is returned as it is.
func (s *Service) ApproveAfterVeto(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if v.Status == StatusApproved {
		return v, nil
	}
	return s.decideVia(ctx, tenantID, id, StatusApproved, nil, ApprovalVetoWindow)
}

// KeepForVeto keeps a draft's files at least until until, so a post scheduled at the end of a veto
// window never finds its video purged. It never shortens an expiry.
func (s *Service) KeepForVeto(ctx context.Context, tenantID, id uuid.UUID, until time.Time) error {
	return s.db.CreativeAsset.Update().
		Where(creativeasset.VersionID(id), creativeasset.TenantID(tenantID), creativeasset.PurgedAtIsNil(),
			creativeasset.ExpiresAtNotNil(), creativeasset.ExpiresAtLT(until)).
		SetExpiresAt(until).
		Exec(ctx)
}

// VideoAsset is a finished video ad's live video file.
func (s *Service) VideoAsset(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeAsset, error) {
	return s.db.CreativeAsset.Query().
		Where(creativeasset.VersionID(id), creativeasset.TenantID(tenantID), creativeasset.KindEQ(AssetVideo), creativeasset.PurgedAtIsNil()).
		First(ctx)
}

// Brief returns one brief of the tenant without its versions (GetBrief loads them too).
func (s *Service) Brief(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeBrief, error) {
	return s.brief(ctx, tenantID, id)
}

// FlaggedForReview lists why a video ad's words need a person before it is posted: restricted
// categories (gate G1) and the compliance review's findings of severity high or review. An empty
// list means nothing was flagged.
func FlaggedForReview(v *ent.CreativeVersion) []string {
	var out []string
	for _, c := range strs(v.Metadata["restricted_categories"]) {
		out = append(out, fmt.Sprintf("it touches a restricted category (%s)", c))
	}
	if findings, ok := v.Metadata["findings"].([]any); ok {
		for _, f := range findings {
			m, _ := f.(map[string]any)
			switch str(m["severity"]) {
			case "high", "review":
				issue := str(m["issue"])
				if issue == "" {
					issue = "the compliance review flagged a line"
				}
				out = append(out, "the compliance review flagged it: "+excerpt(issue, 200))
			}
		}
	}
	return out
}

// SetAutopilotState merges patch into metadata.autopilot of the version as v read it. The update
// is conditional on the version being unchanged since that read (updated_at), so it is also the
// claim of the step that follows: of two workers acting on the same read, one gets ErrConflict.
// Call it only while the version is at rest (script_ready, ready, failed, approved), when the
// Renderer is not writing it.
func SetAutopilotState(ctx context.Context, db *ent.Client, v *ent.CreativeVersion, patch map[string]any) error {
	ap, _ := v.Metadata[MetaAutopilot].(map[string]any)
	ap = copyMap(ap)
	for k, val := range patch {
		ap[k] = val
	}
	meta := copyMap(v.Metadata)
	meta[MetaAutopilot] = ap
	n, err := db.CreativeVersion.Update().
		Where(creativeversion.ID(v.ID), creativeversion.TenantID(v.TenantID), creativeversion.UpdatedAt(v.UpdatedAt)).
		SetMetadata(meta).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: the version changed while its autopilot state was saved", ErrConflict)
	}
	return nil
}
