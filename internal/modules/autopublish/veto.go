package autopublish

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
	"github.com/google/uuid"
)

// The emailed "Cancel this post" link follows the Studio's review-link pattern (/r/{token},
// creative/review.go): the token is the version id, an expiry and an HMAC over them, signed with
// the service key under its own domain, so a review token can never cancel a post or the reverse.
// It expires VetoLinkGrace after the post time; until then a late click is told the post went out.

const (
	vetoDomain   = "marketflow/autopilot-veto/v1"
	vetoMACLen   = 16
	vetoTokenLen = 16 + 4 + vetoMACLen
)

// ErrVetoLink is a link that is malformed, forged or expired. Callers answer all of them alike.
var ErrVetoLink = errors.New("this cancel link is not valid")

// ErrTooLate is a veto after the post reached the platform.
var ErrTooLate = errors.New("the post already went out")

// ErrNotVetoable is a video that is not waiting in a veto window.
var ErrNotVetoable = errors.New("this video is not waiting to be posted by the agent")

// VetoLinks signs and opens cancel links.
type VetoLinks struct {
	key []byte
	now func() time.Time
}

// NewVetoLinks builds the signer; key is the service's HMAC key (the env encryption key).
func NewVetoLinks(key []byte) *VetoLinks {
	if len(key) == 0 {
		return nil
	}
	return &VetoLinks{key: key, now: time.Now}
}

// Sign makes the token for a version, valid until exp.
func (l *VetoLinks) Sign(versionID uuid.UUID, exp time.Time) string {
	buf := make([]byte, 0, vetoTokenLen)
	buf = append(buf, versionID[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(exp.Unix()))
	buf = append(buf, l.mac(buf)...)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// Open checks a token and returns its version id.
func (l *VetoLinks) Open(token string) (uuid.UUID, error) {
	if l == nil {
		return uuid.Nil, ErrVetoLink
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != vetoTokenLen {
		return uuid.Nil, ErrVetoLink
	}
	body, sum := raw[:vetoTokenLen-vetoMACLen], raw[vetoTokenLen-vetoMACLen:]
	if !hmac.Equal(sum, l.mac(body)) {
		return uuid.Nil, ErrVetoLink
	}
	if l.now().After(time.Unix(int64(binary.BigEndian.Uint32(body[16:20])), 0)) {
		return uuid.Nil, ErrVetoLink
	}
	id, _ := uuid.FromBytes(body[:16])
	return id, nil
}

func (l *VetoLinks) mac(body []byte) []byte {
	h := hmac.New(sha256.New, l.key)
	h.Write([]byte(vetoDomain))
	h.Write(body)
	return h.Sum(nil)[:vetoMACLen]
}

// VetoResult is what a veto did.
type VetoResult struct {
	State     string               `json:"state"`
	Cancelled int                  `json:"cancelled"`
	Gone      []publishing.JobView `json:"gone,omitempty"`
	Message   string               `json:"message"`
}

// Veto cancels a veto-window video's post. by is "user:<id>" or ActorVetoLink. A video still on
// its way (not yet scheduled) is stopped at its draft; a scheduled one has its waiting jobs
// cancelled; one whose post already reached the platform answers ErrTooLate with the jobs.
func (p *Pipeline) Veto(ctx context.Context, tenantID, versionID uuid.UUID, by string) (VetoResult, error) {
	v, err := p.DB.CreativeVersion.Query().Where(creativeversion.ID(versionID), creativeversion.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return VetoResult{}, err
	}
	st := readState(v)
	if st == nil {
		return VetoResult{}, ErrNotVetoable
	}
	switch st.State {
	case StateVetoed:
		return VetoResult{State: StateVetoed, Message: "This post was already cancelled."}, nil
	case StateReleased:
		jobs, _ := p.Publish.JobsByID(ctx, tenantID, parseIDs(st.JobIDs))
		return VetoResult{State: StateReleased, Gone: jobs, Message: "The veto window has ended and the post went ahead."}, ErrTooLate
	case StateActive, StateScheduled:
	default:
		return VetoResult{State: st.State, Message: "The agent is no longer handling this video; it waits for you in the studio."}, ErrNotVetoable
	}
	out, err := p.Publish.CancelForVeto(ctx, tenantID, parseIDs(st.JobIDs))
	if err != nil {
		return VetoResult{}, err
	}
	if st.State == StateScheduled && out.Cancelled == 0 && len(out.Gone) > 0 {
		return VetoResult{State: st.State, Gone: out.Gone, Message: "Too late to cancel: the post is already on its way to the platform."}, ErrTooLate
	}
	decision := map[string]any{"outcome": StateVetoed, "by": by, "at": p.now().UTC().Format(time.RFC3339)}
	// The brief carries the veto too, written with an atomic jsonb merge: a video still being made
	// is written by the Renderer, which could put back an older metadata.autopilot, and the
	// pipeline checks the brief before every step.
	if err := markBriefVetoed(ctx, p.DB, tenantID, v.BriefID, decision); err != nil {
		return VetoResult{}, err
	}
	// Re-read: the cancel did not touch the version, but a step may have moved it meanwhile. A
	// version the Renderer is working on keeps its state until the next step reads the brief.
	if v, err = p.DB.CreativeVersion.Get(ctx, versionID); err != nil {
		return VetoResult{}, err
	}
	if st = readState(v); st == nil {
		return VetoResult{}, ErrNotVetoable
	}
	if atRest(v.Status) {
		if err := creative.SetAutopilotState(ctx, p.DB, v, map[string]any{"state": StateVetoed, "decision": decision}); err != nil &&
			!errors.Is(err, creative.ErrConflict) {
			return VetoResult{}, err
		}
	}
	p.audit(ctx, v, "autopilot.post_vetoed", by, map[string]any{"run_id": st.RunID, "agent_id": st.AgentID, "cancelled": out.Cancelled})
	p.trace(ctx, st.RunID, map[string]any{"step": StateVetoed, "version_id": v.ID.String(), "decided_by": by})
	res := VetoResult{State: StateVetoed, Cancelled: out.Cancelled, Gone: out.Gone,
		Message: "Cancelled. Nothing will be posted; the video stays in your studio for you to approve or discard."}
	if len(out.Gone) > 0 {
		res.Message = fmt.Sprintf("Cancelled %d post(s); %d had already reached the platform.", out.Cancelled, len(out.Gone))
	}
	return res, nil
}

// VetoJob cancels the post of a scheduled veto-window video on one account only (owner
// 2026-11-14): the same conditional cancel as a veto, recorded with by as the actor. The video stays
// scheduled while another account's post still waits; once every account's post is cancelled it
// is vetoed like a whole-post veto. The emailed link and the main Cancel button still use Veto,
// which stops every account.
func (p *Pipeline) VetoJob(ctx context.Context, tenantID, versionID, jobID uuid.UUID, by string) (VetoResult, error) {
	v, err := p.DB.CreativeVersion.Query().Where(creativeversion.ID(versionID), creativeversion.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return VetoResult{}, err
	}
	st := readState(v)
	if st == nil {
		return VetoResult{}, ErrNotVetoable
	}
	ids := parseIDs(st.JobIDs)
	known := false
	for _, id := range ids {
		if id == jobID {
			known = true
		}
	}
	if !known {
		return VetoResult{}, &ent.NotFoundError{}
	}
	switch st.State {
	case StateVetoed:
		return VetoResult{State: StateVetoed, Message: "This post was already cancelled."}, nil
	case StateReleased:
		jobs, _ := p.Publish.JobsByID(ctx, tenantID, []uuid.UUID{jobID})
		return VetoResult{State: StateReleased, Gone: jobs, Message: "The veto window has ended and the post went ahead."}, ErrTooLate
	case StateScheduled:
	default:
		return VetoResult{State: st.State, Message: "The agent is no longer handling this video; it waits for you in the studio."}, ErrNotVetoable
	}
	out, err := p.Publish.CancelForVeto(ctx, tenantID, []uuid.UUID{jobID})
	if err != nil {
		return VetoResult{}, err
	}
	if out.Cancelled == 0 && len(out.Gone) > 0 {
		return VetoResult{State: st.State, Gone: out.Gone, Message: "Too late to cancel: this account's post is already on its way to the platform."}, ErrTooLate
	}
	if out.Cancelled > 0 {
		p.audit(ctx, v, "autopilot.account_post_vetoed", by, map[string]any{"run_id": st.RunID, "agent_id": st.AgentID, "job_id": jobID.String()})
		p.trace(ctx, st.RunID, map[string]any{"step": "account_post_vetoed", "version_id": v.ID.String(), "job_id": jobID.String(), "decided_by": by})
	}
	jobs, err := p.Publish.JobsByID(ctx, tenantID, ids)
	if err != nil {
		return VetoResult{}, err
	}
	waiting := 0
	for _, j := range jobs {
		if j.Status != publishing.StatusCancelled {
			waiting++
		}
	}
	if waiting > 0 {
		return VetoResult{State: StateScheduled, Cancelled: out.Cancelled,
			Message: fmt.Sprintf("Cancelled on this account. %d other account(s) still get the post.", waiting)}, nil
	}
	// Every account's post is cancelled now: the video is vetoed, the same as a whole-post veto.
	decision := map[string]any{"outcome": StateVetoed, "by": by, "at": p.now().UTC().Format(time.RFC3339)}
	if err := markBriefVetoed(ctx, p.DB, tenantID, v.BriefID, decision); err != nil {
		return VetoResult{}, err
	}
	if v, err = p.DB.CreativeVersion.Get(ctx, versionID); err != nil {
		return VetoResult{}, err
	}
	if err := creative.SetAutopilotState(ctx, p.DB, v, map[string]any{"state": StateVetoed, "decision": decision}); err != nil &&
		!errors.Is(err, creative.ErrConflict) {
		return VetoResult{}, err
	}
	p.audit(ctx, v, "autopilot.post_vetoed", by, map[string]any{"run_id": st.RunID, "agent_id": st.AgentID, "cancelled": out.Cancelled})
	p.trace(ctx, st.RunID, map[string]any{"step": StateVetoed, "version_id": v.ID.String(), "decided_by": by})
	return VetoResult{State: StateVetoed, Cancelled: out.Cancelled,
		Message: "Cancelled. That was the last account, so nothing will be posted; the video stays in your studio."}, nil
}

// PostView is one veto-window video for the agent dashboard.
type PostView struct {
	VersionID string               `json:"version_id"`
	BriefID   string               `json:"brief_id"`
	AgentID   string               `json:"agent_id"`
	AgentName string               `json:"agent_name,omitempty"`
	Offering  string               `json:"offering"`
	Caption   string               `json:"caption,omitempty"`
	State     string               `json:"state"`
	Reason    string               `json:"reason,omitempty"`
	Status    string               `json:"status"`
	PublishAt *time.Time           `json:"publish_at,omitempty"`
	VetoHours int                  `json:"veto_hours,omitempty"`
	Decision  map[string]any       `json:"decision,omitempty"`
	Steps     []any                `json:"steps,omitempty"`
	Jobs      []publishing.JobView `json:"jobs,omitempty"`
}

// maxListed bounds the dashboard list.
const maxListed = 50

// Posts lists the tenant's veto-window videos of the last 30 days, newest first, optionally of one
// agent and of the given states.
func (p *Pipeline) Posts(ctx context.Context, tenantID uuid.UUID, agentID *uuid.UUID, states []string) ([]PostView, error) {
	if len(states) == 0 {
		states = []string{StateActive, StateScheduled, StateHeld, StateVetoed, StateReleased, StateFailed}
	}
	q := p.DB.CreativeVersion.Query().
		Where(creativeversion.TenantID(tenantID), creativeversion.KindEQ(creative.KindVideoAd),
			creativeversion.CreatedAtGT(p.now().AddDate(0, 0, -30)), autopilotStateIn(states...))
	if agentID != nil {
		q = q.Where(autopilotAgentIs(agentID.String()))
	}
	rows, err := q.Order(ent.Desc(creativeversion.FieldCreatedAt)).Limit(maxListed).All(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	out := make([]PostView, 0, len(rows))
	for _, v := range rows {
		st := readState(v)
		if st == nil {
			continue
		}
		view := PostView{VersionID: v.ID.String(), BriefID: v.BriefID.String(), AgentID: st.AgentID, State: st.State,
			Reason: st.Reason, Status: v.Status, VetoHours: st.VetoHours}
		if !st.PublishAt.IsZero() {
			at := st.PublishAt
			view.PublishAt = &at
		}
		view.Decision, _ = st.Raw["decision"].(map[string]any)
		view.Steps, _ = st.Raw["steps"].([]any)
		if b, err := p.Studio.Brief(ctx, tenantID, v.BriefID); err == nil {
			view.Offering = offeringName(b)
			view.Caption = Caption(v, b)
		}
		if name, ok := names[st.AgentID]; ok {
			view.AgentName = name
		} else if aid, err := uuid.Parse(st.AgentID); err == nil {
			if a, err := p.DB.CustomAgent.Query().Where(customagent.IDEQ(aid), customagent.TenantID(tenantID)).Only(ctx); err == nil {
				names[st.AgentID], view.AgentName = a.Name, a.Name
			}
		}
		if len(st.JobIDs) > 0 {
			view.Jobs, _ = p.Publish.JobsByID(ctx, tenantID, parseIDs(st.JobIDs))
			view.Caption = captionOf(view.Jobs, view.Caption)
		}
		out = append(out, view)
	}
	return out, nil
}

// PublicPost is what the public cancel page shows: enough to recognise the post, nothing more.
type PublicPost struct {
	Business  string     `json:"business"`
	Offering  string     `json:"offering"`
	Caption   string     `json:"caption,omitempty"`
	Places    []string   `json:"places"`
	PublishAt *time.Time `json:"publish_at,omitempty"`
	State     string     `json:"state"`
	CanCancel bool       `json:"can_cancel"`
}

// PublicView reads the post a cancel link points at.
func (p *Pipeline) PublicView(ctx context.Context, versionID uuid.UUID) (PublicPost, uuid.UUID, error) {
	v, err := p.DB.CreativeVersion.Get(ctx, versionID)
	if err != nil {
		return PublicPost{}, uuid.Nil, ErrVetoLink
	}
	st := readState(v)
	if st == nil {
		return PublicPost{}, uuid.Nil, ErrVetoLink
	}
	out := PublicPost{State: st.State, CanCancel: st.State == StateScheduled || st.State == StateActive}
	if t, err := p.DB.Tenant.Get(ctx, v.TenantID); err == nil {
		out.Business = t.Name
	}
	if b, err := p.Studio.Brief(ctx, v.TenantID, v.BriefID); err == nil {
		out.Offering, out.Caption = offeringName(b), Caption(v, b)
	}
	if !st.PublishAt.IsZero() {
		at := st.PublishAt
		out.PublishAt = &at
	}
	if jobs, err := p.Publish.JobsByID(ctx, v.TenantID, parseIDs(st.JobIDs)); err == nil {
		out.Places = placeNames(ctx, p.DB, jobs)
		out.Caption = captionOf(jobs, out.Caption)
	}
	if out.Places == nil {
		out.Places = []string{}
	}
	return out, v.TenantID, nil
}

func parseUUID(s string) (uuid.UUID, error) { return uuid.Parse(strings.TrimSpace(s)) }

// briefVetoKey is the brief metadata key of a veto.
const briefVetoKey = "autopilot_vetoed"

// markBriefVetoed merges the veto into the brief's metadata in one statement.
func markBriefVetoed(ctx context.Context, db *ent.Client, tenantID, briefID uuid.UUID, decision map[string]any) error {
	raw, err := json.Marshal(map[string]any{briefVetoKey: decision})
	if err != nil {
		return err
	}
	value := entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("(COALESCE(" + creativebrief.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(raw))
		b.WriteString("::jsonb)")
	})
	return db.CreativeBrief.Update().Where(creativebrief.ID(briefID), creativebrief.TenantID(tenantID)).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(creativebrief.FieldMetadata, value) }).
		Exec(ctx)
}

// briefVeto is the veto recorded on the brief, nil when none.
func briefVeto(b *ent.CreativeBrief) map[string]any {
	if b == nil {
		return nil
	}
	d, _ := b.Metadata[briefVetoKey].(map[string]any)
	return d
}

// atRest reports whether the Renderer leaves a version alone in this status.
func atRest(status string) bool {
	switch status {
	case creative.StatusQueued, creative.StatusRendering:
		return false
	}
	return true
}
