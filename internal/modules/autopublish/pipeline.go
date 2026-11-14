package autopublish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/config"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Deps are what the pipeline works through. Studio must carry the daily render quota
// (creative.Service.WithQuota) so the render cap applies; Quoter must reach marketflow-ai.
type Deps struct {
	DB       *ent.Client
	Studio   *creative.Service
	Quoter   *creative.Renderer
	Publish  *publishing.EntStore
	Gate     custom_agents.AutoPublishGate
	Notif    *notifications.Client
	Links    *VetoLinks
	AfterUse *creative.AfterUse
	AppURL   string
	Config   config.AutopilotConfig
	Logger   *zap.Logger
}

// Pipeline carries veto-window videos on. It implements creative.AutopilotHook.
type Pipeline struct {
	Deps
	logger *zap.Logger
	now    func() time.Time
}

// New builds a pipeline.
func New(d Deps) *Pipeline {
	if d.Config.QuoteRetries <= 0 {
		d.Config.QuoteRetries = 5
	}
	if d.Config.PipelineInterval <= 0 {
		d.Config.PipelineInterval = time.Minute
	}
	if d.Config.PipelineSettle <= 0 {
		d.Config.PipelineSettle = 2 * time.Minute
	}
	if d.Config.OutageHandover <= 0 {
		d.Config.OutageHandover = 24 * time.Hour
	}
	return &Pipeline{Deps: d, logger: d.Logger.Named("autopublish"), now: time.Now}
}

// Settled implements creative.AutopilotHook: a step settled, so take the next one.
func (p *Pipeline) Settled(ctx context.Context, tenantID, versionID uuid.UUID, event string) {
	if err := p.Advance(ctx, tenantID, versionID); err != nil {
		p.logger.Warn("autopilot step not taken", zap.String("version_id", versionID.String()), zap.String("event", event), zap.Error(err))
	}
}

// Advance takes the next step of one veto-window video. It is safe to call at any time and from
// any replica: it reads the current state, and each step is claimed by a conditional update.
func (p *Pipeline) Advance(ctx context.Context, tenantID, id uuid.UUID) error {
	// One plan and terms answer per tenant per sweep (or per call, outside a sweep).
	ctx = withGateCache(ctx)
	v, err :=p.DB.CreativeVersion.Query().Where(creativeversion.ID(id), creativeversion.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return err
	}
	st := readState(v)
	if st == nil {
		return nil
	}
	switch st.State {
	case StateScheduled:
		return p.release(ctx, v, st)
	case StateActive:
	default:
		return nil
	}
	// Owner 2026-11-14: while the plan or terms cannot be read, the video waits for the next sweep
	// instead of falling back to approve-each on a guess.
	if !p.gateCheck(ctx, tenantID).known {
		return nil
	}
	brief, err := p.Studio.Brief(ctx, tenantID, v.BriefID)
	if err != nil {
		return err
	}
	if d := briefVeto(brief); d != nil {
		// Vetoed while the video was being made: it stops at its draft (the veto is already audited).
		if atRest(v.Status) {
			return ignoreConflict(creative.SetAutopilotState(ctx, p.DB, v, map[string]any{"state": StateVetoed, "decision": d}))
		}
		return nil
	}
	agent, cfg, agentOK, agentWhy := p.agent(ctx, tenantID, st.AgentID)
	snap := snapshot{Status: v.Status, Presenter: creative.IsPresenterVersion(v), AgentOK: agentOK, AgentWhy: agentWhy, Flags: creative.FlaggedForReview(v)}
	_, snap.HasScript = v.Copy["script"]
	if sb, ok := v.Metadata["storyboard"].(map[string]any); ok && sb["stills"] != nil {
		snap.Storyboard = true
		at, _ := sb["approved_at"].(string)
		snap.StoryboardApproved = at != ""
	}
	snap.StoryboardError, _ = v.Metadata["storyboard_error"].(string)
	if t, err := p.DB.Tenant.Get(ctx, tenantID); err == nil {
		// The tenant's "Always ask me first for" list: its categories and words, and the restricted
		// categories it keeps, matched on the offering's own text too (owner 2026-11-14).
		snap.HeldCategory = custom_agents.HeldFor(custom_agents.HoldCategories(t), brief.Offering, creative.RestrictedCategories)
	}

	step, why := nextStep(snap)
	switch step {
	case stepWait:
		return nil
	case stepHandOver:
		return p.settle(ctx, v, st, StateHandedOver, why, nil)
	case stepFailed:
		reason := v.Error
		if reason == "" {
			reason = "the video could not be made"
		}
		if err := p.settle(ctx, v, st, StateFailed, reason, nil); err != nil {
			return err
		}
		p.tellHeld(ctx, v, brief, agent, "it could not be finished: "+reason)
		return nil
	case stepHold:
		return p.hold(ctx, v, st, brief, agent, why)
	case stepStoryboard:
		if why, err := p.ppbHeld(ctx, v); err != nil {
			return err
		} else if why != "" {
			return p.hold(ctx, v, st, brief, agent, why)
		}
		if used, err := custom_agents.CreditsThisMonth(ctx, p.DB, tenantID, agent.ID, p.now()); err == nil && used >= cfg.CreditCap {
			return p.hold(ctx, v, st, brief, agent, fmt.Sprintf("the agent reached this month's credit ceiling of %d", cfg.CreditCap))
		}
		if !p.claim(ctx, v, st, "storyboard_requested", agentActor(st.AgentID), nil) {
			return nil
		}
		if _, err := p.Studio.MakeStoryboard(ctx, tenantID, id); err != nil {
			return ignoreConflict(err)
		}
		p.tookStep(ctx, v, st, "storyboard_requested", nil)
		return nil
	case stepApproveStoryboard:
		if !p.claim(ctx, v, st, "storyboard_approved", agentActor(st.AgentID), nil) {
			return nil
		}
		if _, err := p.Studio.ApproveStoryboardByAgent(ctx, tenantID, id, agent.ID); err != nil {
			return ignoreConflict(err)
		}
		p.tookStep(ctx, v, st, "storyboard_approved", nil)
		// Approved: the render is the next step, taken now rather than on the next sweep.
		return p.Advance(ctx, tenantID, id)
	case stepRender:
		return p.render(ctx, v, st, brief, agent, cfg)
	case stepSchedule:
		return p.schedule(ctx, v, st, brief, agent, cfg)
	}
	return nil
}

// agent reads the agent and whether the veto-window mode still applies to it.
func (p *Pipeline) agent(ctx context.Context, tenantID uuid.UUID, agentID string) (*ent.CustomAgent, custom_agents.AutopilotConfig, bool, string) {
	var cfg custom_agents.AutopilotConfig
	aid, err := uuid.Parse(agentID)
	if err != nil {
		return nil, cfg, false, "the agent is unknown"
	}
	a, err := p.DB.CustomAgent.Query().Where(customagent.IDEQ(aid), customagent.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return nil, cfg, false, "the agent was deleted"
	}
	if cfg, _, err = custom_agents.ReadAutopilot(a); err != nil {
		return a, cfg, false, "the agent's settings could not be read"
	}
	switch {
	case !a.IsActive:
		return a, cfg, false, "the agent was switched off"
	case cfg.Approval != custom_agents.AutoPublish:
		return a, cfg, false, "the agent is now set to wait for your approval of each ad"
	}
	if gc := p.gateCheck(ctx, tenantID); gc.why != "" {
		return a, cfg, false, gc.why
	}
	if cfg.VetoHours < custom_agents.MinVetoHours || cfg.VetoHours > custom_agents.MaxVetoHours {
		cfg.VetoHours = custom_agents.DefaultVeto
	}
	return a, cfg, true, ""
}

// render checks the month's ceiling against the render quote, then queues the render through the
// Studio's RenderVideoAd: the same charge, refund and daily cap as a person's render.
func (p *Pipeline) render(ctx context.Context, v *ent.CreativeVersion, st *State, brief *ent.CreativeBrief, agent *ent.CustomAgent, cfg custom_agents.AutopilotConfig) error {
	today := p.now().UTC().Format("2006-01-02")
	if day, _ := st.Raw["daily_cap_day"].(string); day == today {
		return nil // today's renders are used up; tomorrow's sweep tries again
	}
	// A pharmacy's medicine ad waits for its PPB approval reference before anything is quoted or
	// spent (pharmacy hand-off 2026-11-14).
	if why, err := p.ppbHeld(ctx, v); err != nil {
		return err
	} else if why != "" {
		return p.hold(ctx, v, st, brief, agent, why)
	}
	q, err := p.Quoter.QuoteRenderCredits(ctx, v)
	if err != nil {
		tries := intOf(st.Raw["quote_errors"]) + 1
		if tries >= p.Config.QuoteRetries {
			return p.hold(ctx, v, st, brief, agent, "the render price could not be worked out")
		}
		p.logger.Warn("autopilot render quote failed", zap.String("version_id", v.ID.String()), zap.Error(err))
		return ignoreConflict(creative.SetAutopilotState(ctx, p.DB, v, map[string]any{"quote_errors": tries}))
	}
	used, err := custom_agents.CreditsThisMonth(ctx, p.DB, v.TenantID, agent.ID, p.now())
	if err != nil {
		return err
	}
	switch {
	case !cfg.CreditsFit(used, q.CreditsNeeded):
		return p.hold(ctx, v, st, brief, agent, fmt.Sprintf("the video needs %d credits and would pass the agent's monthly ceiling of %d (%d used)", q.CreditsNeeded, cfg.CreditCap, used))
	case !q.Enough:
		return p.hold(ctx, v, st, brief, agent, fmt.Sprintf("the video needs %d credits and your balance is %d", q.CreditsNeeded, q.Balance))
	}
	if !p.claim(ctx, v, st, "render_requested", agentActor(st.AgentID), map[string]any{"credits_quoted": q.CreditsNeeded}) {
		return nil
	}
	_, err = p.Studio.RenderVideoAd(ctx, v.TenantID, v.ID)
	if errors.Is(err, creative.ErrDailyCap) {
		// Today's renders are used up: nothing was charged, and the sweep tries again tomorrow.
		p.logger.Info("autopilot render waits for the daily cap", zap.String("version_id", v.ID.String()))
		if fresh, err := p.DB.CreativeVersion.Get(ctx, v.ID); err == nil {
			_ = ignoreConflict(creative.SetAutopilotState(ctx, p.DB, fresh, map[string]any{"daily_cap_day": today}))
		}
		return nil
	}
	if err != nil {
		return ignoreConflict(err)
	}
	p.tookStep(ctx, v, st, "render_requested", map[string]any{"credits_quoted": q.CreditsNeeded})
	return nil
}

// schedule posts the finished video to the agent's accounts at now + VetoHours and sends the
// heads-up. Nowhere to post leaves a ready draft for the owner.
func (p *Pipeline) schedule(ctx context.Context, v *ent.CreativeVersion, st *State, brief *ent.CreativeBrief, agent *ent.CustomAgent, cfg custom_agents.AutopilotConfig) error {
	// The reference may have been removed after the render: no post is scheduled without it.
	if why, err := p.ppbHeld(ctx, v); err != nil {
		return err
	} else if why != "" {
		return p.hold(ctx, v, st, brief, agent, why)
	}
	// No post without its heads-up: without a way to email the cancel link, the owner decides.
	if p.Links == nil || p.Notif == nil {
		return p.hold(ctx, v, st, brief, agent, "the heads-up email with the cancel link cannot be sent right now")
	}
	recipients := p.headsUpRecipients(ctx, v.TenantID)
	if len(recipients) == 0 {
		return p.hold(ctx, v, st, brief, agent, "there is no owner or admin email to send the heads-up with the cancel link to")
	}
	asset, err := p.Studio.VideoAsset(ctx, v.TenantID, v.ID)
	if err != nil {
		return p.hold(ctx, v, st, brief, agent, "the finished video file is missing")
	}
	hours := cfg.VetoHours
	at := p.now().UTC().Add(time.Duration(hours) * time.Hour).Truncate(time.Second)
	// Claim first: the claim moves updated_at, so a second replica stops here.
	if !p.claim(ctx, v, st, "post_scheduling", agentActor(st.AgentID), nil) {
		return nil
	}
	v, err = p.DB.CreativeVersion.Get(ctx, v.ID)
	if err != nil {
		return err
	}
	st = readState(v)
	out, err := p.Publish.ScheduleForVeto(ctx, publishing.VetoRequest{
		TenantID: v.TenantID, AssetID: asset.ID, VersionID: v.ID, AgentID: agent.ID,
		AccountIDs: cfg.AccountIDs, Platforms: cfg.Platforms, Caption: Caption(v, brief), At: at,
		// The owner's AI label choice (owner 2026-11-14): off leaves out the platforms' AI flag.
		NoAILabel: !creative.TenantAILabel(ctx, p.DB, v.TenantID),
	})
	if err != nil {
		return err
	}
	if len(out.Jobs) == 0 {
		return p.hold(ctx, v, st, brief, agent, "none of the agent's accounts can take the post now ("+skippedText(out.Skipped)+")")
	}
	// A veto that landed while the jobs were written still wins: they are cancelled at once.
	if b, err := p.Studio.Brief(ctx, v.TenantID, v.BriefID); err == nil && briefVeto(b) != nil {
		jobIDs := make([]uuid.UUID, 0, len(out.Jobs))
		for _, j := range out.Jobs {
			if id, err := uuid.Parse(j.ID); err == nil {
				jobIDs = append(jobIDs, id)
			}
		}
		if _, err := p.Publish.CancelForVeto(ctx, v.TenantID, jobIDs); err != nil {
			return err
		}
		return ignoreConflict(creative.SetAutopilotState(ctx, p.DB, v, map[string]any{"state": StateVetoed, "decision": briefVeto(b)}))
	}
	// The draft's files stay at least until the post, and a draft's lifetime after it.
	if err := p.Studio.KeepForVeto(ctx, v.TenantID, v.ID, at.Add(creative.DraftTTL)); err != nil {
		p.logger.Warn("autopilot video expiry not extended", zap.String("version_id", v.ID.String()), zap.Error(err))
	}
	ids := make([]any, 0, len(out.Jobs))
	for _, j := range out.Jobs {
		ids = append(ids, j.ID)
		// Jobs kept from an earlier attempt of this step hold their own time: the window ends then.
		if j.PlannedAt.Before(at) {
			at = j.PlannedAt.UTC()
		}
	}
	patch := map[string]any{"state": StateScheduled, "publish_at": at.Format(time.RFC3339), "veto_hours": hours, "job_ids": ids}
	if len(out.Skipped) > 0 {
		patch["skipped"] = out.Skipped
	}
	if !p.claim(ctx, v, st, "post_scheduled", agentActor(st.AgentID), patch) {
		// The jobs exist. A later sweep schedules again and ScheduleForVeto finds this video's own
		// jobs by their dedupe keys, so there is still one post per account.
		return nil
	}
	p.audit(ctx, v, "autopilot.post_scheduled", agentActor(st.AgentID), map[string]any{"publish_at": at.Format(time.RFC3339), "jobs": ids, "run_id": st.RunID})
	p.trace(ctx, st.RunID, map[string]any{"step": "post_scheduled", "version_id": v.ID.String(), "publish_at": at.Format(time.RFC3339), "by": agentActor(st.AgentID)})
	p.tellScheduled(ctx, v, brief, agent, cfg, out, at, recipients)
	return nil
}

// release closes a veto window that has ended: the video is approved with "veto_window_elapsed"
// as the approver, unless every post was cancelled meanwhile (from the calendar), which counts as
// a veto. A person who approved the video in the Studio meanwhile stays its approver.
func (p *Pipeline) release(ctx context.Context, v *ent.CreativeVersion, st *State) error {
	ids := parseIDs(st.JobIDs)
	// Owner 2026-11-14: an agent switched off, set back to approve-each or deleted while its post
	// waits in the window has that post cancelled at once, and the owner is told.
	if why := p.agentOff(ctx, v.TenantID, st.AgentID); why != "" {
		return p.cancelForAgent(ctx, v, st, ids, ActorAgentOff, why)
	}
	// Owner 2026-11-14: a definite lapsed plan or new terms the tenant has not accepted cancel the
	// waiting posts the same way, at once, with their own reason in the email.
	gc := p.gateCheck(ctx, v.TenantID)
	if gc.known && gc.why != "" {
		return p.cancelForAgent(ctx, v, st, ids, ActorAutoPublishLapsed, gc.why)
	}
	switch v.Status {
	case creative.StatusRejected, creative.StatusSuperseded, creative.StatusFailed:
		// Discarded or replaced in the Studio during the window: its posts must not go out.
		if _, err := p.Publish.CancelForVeto(ctx, v.TenantID, ids); err != nil {
			return err
		}
		return p.decide(ctx, v, st, StateVetoed, "studio", "the video was discarded or replaced in the studio")
	}
	if !gc.known {
		// Owner 2026-11-14: an answer that is not definite (an outage, a timeout) never cancels.
		// Nothing is released either: jobs about to fall due are moved on, so a post whose window
		// ends during the outage waits until a check succeeds. After OutageHandover from the first
		// failed check for this post, the post is handed to the owner instead (its jobs are
		// cancelled, so nothing goes out unchecked) and the owner is emailed once.
		first, seen := planCheckFailedAt(st)
		if !seen {
			first = p.now().UTC()
			if err := creative.SetAutopilotState(ctx, p.DB, v, map[string]any{planCheckFailedKey: first.Format(time.RFC3339)}); err != nil {
				return ignoreConflict(err)
			}
		} else if p.now().Sub(first) >= p.Config.OutageHandover {
			return p.holdScheduled(ctx, v, st, ids, fmt.Sprintf("your plan could not be checked for %s, so the agent will not post it by itself", humanHours(p.Config.OutageHandover)))
		}
		if _, err := p.Publish.PostponeForVeto(ctx, v.TenantID, ids, p.now().Add(3*p.Config.PipelineInterval)); err != nil {
			return err
		}
		return nil
	}
	if _, seen := planCheckFailedAt(st); seen {
		// The check succeeds again: a later outage counts from its own first failure.
		if err := creative.SetAutopilotState(ctx, p.DB, v, map[string]any{planCheckFailedKey: nil}); err != nil {
			return ignoreConflict(err)
		}
		fresh, err := p.DB.CreativeVersion.Get(ctx, v.ID)
		if err != nil {
			return err
		}
		v, st = fresh, readState(fresh)
	}
	if st.PublishAt.IsZero() || p.now().Before(st.PublishAt) {
		return nil
	}
	jobs, err := p.Publish.JobsByID(ctx, v.TenantID, ids)
	if err != nil {
		return err
	}
	allCancelled := len(jobs) > 0
	for _, j := range jobs {
		if j.Status != publishing.StatusCancelled {
			allCancelled = false
		}
	}
	if allCancelled {
		if b, err := p.Studio.Brief(ctx, v.TenantID, v.BriefID); err == nil && briefVeto(b) != nil {
			// The veto already wrote its decision and audit; only the state is left to settle.
			return ignoreConflict(creative.SetAutopilotState(ctx, p.DB, v, map[string]any{"state": StateVetoed, "decision": briefVeto(b)}))
		}
		return p.decide(ctx, v, st, StateVetoed, "calendar", "every post was cancelled from the calendar")
	}
	by := ActorVetoWindow
	if v.Status == creative.StatusApproved {
		if v.ApprovedBy != nil {
			by = "user:" + v.ApprovedBy.String()
		}
	} else if _, err := p.Studio.ApproveAfterVeto(ctx, v.TenantID, v.ID); err != nil {
		if errors.Is(err, creative.ErrPPBApprovalRequired) {
			// A pharmacy's medicine ad without its PPB approval reference is never approved by the
			// window: its waiting posts are cancelled and it waits for the owner.
			return p.holdScheduled(ctx, v, st, ids, ppbReason(err))
		}
		return ignoreConflict(err)
	}
	if v, err = p.DB.CreativeVersion.Get(ctx, v.ID); err != nil {
		return err
	}
	if err := p.decide(ctx, v, readState(v), StateReleased, by, ""); err != nil {
		return err
	}
	// The video is now kept; once its posts are done its 48-hour removal countdown starts.
	if p.AfterUse != nil {
		if a, err := p.Studio.VideoAsset(ctx, v.TenantID, v.ID); err == nil {
			_ = p.AfterUse.Check(ctx, a.ID)
		}
	}
	return nil
}

// ActorAgentOff is recorded as the canceller when the agent was turned off during the window.
const ActorAgentOff = "agent_turned_off"

// ActorAutoPublishLapsed is recorded as the canceller when the tenant's plan or terms stopped
// allowing the veto-window mode during the window.
const ActorAutoPublishLapsed = "auto_publish_lapsed"

// The reasons in the cancel email when the tenant no longer may auto-publish (owner 2026-11-14).
const (
	WhyPlanLapsed   = "your plan no longer includes auto-publish"
	WhyTermsChanged = "new auto-publish terms need your acceptance"
)

// planCheckFailedKey records on metadata.autopilot when the plan check first failed for a
// scheduled post, cleared once a check succeeds (owner 2026-11-14).
const planCheckFailedKey = "plan_check_failed_at"

// planCheckFailedAt reads planCheckFailedKey; seen is false when it is unset.
func planCheckFailedAt(st *State) (time.Time, bool) {
	s, _ := st.Raw[planCheckFailedKey].(string)
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// humanHours writes a duration for the owner's email: "24 hours", "90 minutes".
func humanHours(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		if d == time.Hour {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}

// gateAnswer is one tenant's plan and terms answer. why is set when the tenant definitely no
// longer may auto-publish; known is false when the answer could not be read (an outage, a
// timeout, the tenant unreadable), and then nothing is cancelled for it.
type gateAnswer struct {
	why   string
	known bool
}

type gateCacheKey struct{}

// withGateCache gives ctx a per-sweep cache of gate answers, unless it already has one.
func withGateCache(ctx context.Context) context.Context {
	if _, ok := ctx.Value(gateCacheKey{}).(map[uuid.UUID]gateAnswer); ok {
		return ctx
	}
	return context.WithValue(ctx, gateCacheKey{}, map[uuid.UUID]gateAnswer{})
}

// gateCheck reads the tenant's plan and terms once per sweep (owner 2026-11-14): every video of
// the tenant in the same sweep shares the answer. The sweep walks its videos one at a time, so
// the map needs no lock.
func (p *Pipeline) gateCheck(ctx context.Context, tenantID uuid.UUID) gateAnswer {
	cache, _ := ctx.Value(gateCacheKey{}).(map[uuid.UUID]gateAnswer)
	if a, ok := cache[tenantID]; ok {
		return a
	}
	a := p.readGate(ctx, tenantID)
	if cache != nil {
		cache[tenantID] = a
	}
	return a
}

// readGate asks the gate: the plan feature (strict, and definite only when the snapshot was read)
// and the terms with their effective date.
func (p *Pipeline) readGate(ctx context.Context, tenantID uuid.UUID) gateAnswer {
	st, err := p.Gate.Status(ctx, tenantID)
	switch {
	case err != nil || st.PlanUnknown:
		return gateAnswer{}
	case !st.PlanAllows:
		return gateAnswer{why: WhyPlanLapsed, known: true}
	case st.TermsUnknown:
		// auth-api's terms could not be read and nothing was known before: neither publish nor
		// cancel on it; the next sweep asks again.
		return gateAnswer{}
	case !st.TermsAccepted:
		return gateAnswer{why: WhyTermsChanged, known: true}
	}
	return gateAnswer{known: true}
}

// agentOff returns why the agent no longer posts by itself (switched off, set back to
// approve-each, deleted), or "" while it still does. A lapsed plan or new terms are checked
// apart (autoPublishLapsed).
func (p *Pipeline) agentOff(ctx context.Context, tenantID uuid.UUID, agentID string) string {
	aid, err := uuid.Parse(agentID)
	if err != nil {
		return "the agent is unknown"
	}
	a, err := p.DB.CustomAgent.Query().Where(customagent.IDEQ(aid), customagent.TenantID(tenantID)).Only(ctx)
	if ent.IsNotFound(err) {
		return "the agent was deleted"
	}
	if err != nil {
		return "" // unreadable now: the next sweep checks again
	}
	if !a.IsActive {
		return "the agent was switched off"
	}
	if cfg, _, err := custom_agents.ReadAutopilot(a); err == nil && cfg.Approval != custom_agents.AutoPublish {
		return "the agent was set back to wait for your approval of each ad"
	}
	return ""
}

// cancelForAgent cancels the waiting posts of a video whose agent was turned off, or whose tenant
// no longer may auto-publish, records it apart from the agent's steps with by as the canceller,
// and emails the owner and admins what was cancelled.
func (p *Pipeline) cancelForAgent(ctx context.Context, v *ent.CreativeVersion, st *State, ids []uuid.UUID, by, why string) error {
	out, err := p.Publish.CancelForVeto(ctx, v.TenantID, ids)
	if err != nil {
		return err
	}
	if err := p.decide(ctx, v, st, StateVetoed, by, why); err != nil {
		return err
	}
	if out.Cancelled > 0 {
		brief, _ := p.Studio.Brief(ctx, v.TenantID, v.BriefID)
		p.tellCancelled(ctx, v, brief, why, out)
	}
	return nil
}

// AgentChanged cancels the waiting veto-window posts of an agent that was switched off, set back
// to approve-each or deleted (custom_agents.WithAgentChanged). The sweep does the same within a
// minute if this call is missed.
func (p *Pipeline) AgentChanged(ctx context.Context, tenantID, agentID uuid.UUID) {
	p.advanceWaiting(ctx, tenantID, "agent change", autopilotAgentIs(agentID.String()))
}

// AutoPublishLapsed cancels the tenant's veto-window posts still waiting when a run found that the
// plan or the terms no longer allow the mode (owner 2026-11-14). Each post's release checks the
// plan and terms again before it cancels, and the sweep does the same within a minute if this
// call is missed.
func (p *Pipeline) AutoPublishLapsed(ctx context.Context, tenantID uuid.UUID) {
	p.advanceWaiting(ctx, tenantID, "auto-publish lapsed")
}

// advanceWaiting advances the tenant's scheduled veto-window videos that match where, so each one
// whose agent or tenant no longer allows the post is cancelled now.
func (p *Pipeline) advanceWaiting(ctx context.Context, tenantID uuid.UUID, what string, where ...func(*entsql.Selector)) {
	ctx = withGateCache(ctx)
	q :=p.DB.CreativeVersion.Query().
		Where(creativeversion.TenantID(tenantID), creativeversion.KindEQ(creative.KindVideoAd),
			creativeversion.CreatedAtGT(p.now().AddDate(0, 0, -30)), autopilotStateIn(StateScheduled))
	for _, w := range where {
		q = q.Where(w)
	}
	rows, err := q.Limit(sweepBatch).All(ctx)
	if err != nil {
		p.logger.Warn("autopilot "+what+": posts not read", zap.String("tenant", tenantID.String()), zap.Error(err))
		return
	}
	for _, v := range rows {
		if err := p.Advance(ctx, tenantID, v.ID); err != nil {
			p.logger.Warn("autopilot "+what+": post not cancelled", zap.String("version_id", v.ID.String()), zap.Error(err))
		}
	}
}

// autopilotAgentIs matches versions whose metadata.autopilot.agent_id is id.
func autopilotAgentIs(id string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString(s.C(creativeversion.FieldMetadata) + "->'" + creative.MetaAutopilot + "'->>'agent_id' = ").Arg(id)
		}))
	}
}

// decide records the human side of the window, apart from the agent's steps: who vetoed, or
// veto_window_elapsed.
func (p *Pipeline) decide(ctx context.Context, v *ent.CreativeVersion, st *State, state, by, note string) error {
	at := p.now().UTC().Format(time.RFC3339)
	decision := map[string]any{"outcome": state, "by": by, "at": at}
	if note != "" {
		decision["note"] = note
	}
	if err := creative.SetAutopilotState(ctx, p.DB, v, map[string]any{"state": state, "decision": decision}); err != nil {
		return ignoreConflict(err)
	}
	event := "autopilot.post_released"
	if state == StateVetoed {
		event = "autopilot.post_vetoed"
	}
	p.audit(ctx, v, event, by, map[string]any{"run_id": st.RunID, "agent_id": st.AgentID, "note": note})
	p.trace(ctx, st.RunID, map[string]any{"step": state, "version_id": v.ID.String(), "decided_by": by, "at": at})
	return nil
}

// ppbHeld returns why a pharmacy's medicine ad must wait for its PPB approval reference, or ""
// when it need not (creative.CheckPPBApprovalForVersion, pharmacy hand-off 2026-11-14: only a
// version whose words promote the medicine is held, a plain availability post is not).
func (p *Pipeline) ppbHeld(ctx context.Context, v *ent.CreativeVersion) (string, error) {
	_, err := p.Studio.CheckPPBApprovalForVersion(ctx, v.TenantID, v)
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, creative.ErrPPBApprovalRequired):
		return ppbReason(err), nil
	}
	return "", err
}

// ppbReason is the hold reason for the owner's email; the email carries the Studio link where the
// reference is entered.
func ppbReason(err error) string {
	var req *creative.PPBApprovalRequiredError
	if errors.As(err, &req) && req.OfferingName != "" {
		return fmt.Sprintf("it advertises a medicine (%s) and needs its Pharmacy and Poisons Board approval reference. Enter the reference in the studio, then approve the ad", req.OfferingName)
	}
	return "it advertises a medicine and needs its Pharmacy and Poisons Board approval reference. Enter the reference in the studio, then approve the ad"
}

// holdScheduled cancels a scheduled video's waiting posts and hands it to the owner with why.
func (p *Pipeline) holdScheduled(ctx context.Context, v *ent.CreativeVersion, st *State, ids []uuid.UUID, why string) error {
	if _, err := p.Publish.CancelForVeto(ctx, v.TenantID, ids); err != nil {
		return err
	}
	brief, _ := p.Studio.Brief(ctx, v.TenantID, v.BriefID)
	return p.hold(ctx, v, st, brief, nil, why)
}

// hold moves the video to approve-each: it waits for a person and the owner is emailed why.
func (p *Pipeline) hold(ctx context.Context, v *ent.CreativeVersion, st *State, brief *ent.CreativeBrief, agent *ent.CustomAgent, reason string) error {
	if err := p.settle(ctx, v, st, StateHeld, reason, nil); err != nil {
		return err
	}
	p.tellHeld(ctx, v, brief, agent, reason)
	return nil
}

// settle ends the agent's part with a final state and logs it as the agent's step.
func (p *Pipeline) settle(ctx context.Context, v *ent.CreativeVersion, st *State, state, reason string, extra map[string]any) error {
	patch := map[string]any{"state": state, "reason": reason}
	for k, val := range extra {
		patch[k] = val
	}
	if !p.claim(ctx, v, st, state, agentActor(st.AgentID), patch) {
		return nil
	}
	p.audit(ctx, v, "autopilot."+state, agentActor(st.AgentID), map[string]any{"reason": reason, "run_id": st.RunID})
	p.trace(ctx, st.RunID, map[string]any{"step": state, "version_id": v.ID.String(), "reason": reason, "by": agentActor(st.AgentID)})
	return nil
}

// claim logs a step on the version, conditional on the version being as v read it; false when
// another replica moved first. The step is also written to AuditLog with the actor.
func (p *Pipeline) claim(ctx context.Context, v *ent.CreativeVersion, st *State, step, actor string, patch map[string]any) bool {
	entry := map[string]any{"step": step, "by": actor, "at": p.now().UTC().Format(time.RFC3339)}
	full := map[string]any{"steps": withStep(st.Raw, entry)}
	for k, val := range patch {
		full[k] = val
	}
	if err := creative.SetAutopilotState(ctx, p.DB, v, full); err != nil {
		if !errors.Is(err, creative.ErrConflict) {
			p.logger.Warn("autopilot step not recorded", zap.String("version_id", v.ID.String()), zap.String("step", step), zap.Error(err))
		}
		return false
	}
	return true
}

// tookStep records a step the agent took in AuditLog and on the run's trace, once it happened.
func (p *Pipeline) tookStep(ctx context.Context, v *ent.CreativeVersion, st *State, step string, extra map[string]any) {
	actor := agentActor(st.AgentID)
	payload := map[string]any{"run_id": st.RunID}
	entry := map[string]any{"step": step, "version_id": v.ID.String(), "by": actor}
	for k, val := range extra {
		payload[k], entry[k] = val, val
	}
	p.audit(ctx, v, "autopilot."+step, actor, payload)
	p.trace(ctx, st.RunID, entry)
}

// audit writes one AuditLog row about the version.
func (p *Pipeline) audit(ctx context.Context, v *ent.CreativeVersion, event, actor string, payload map[string]any) {
	payload["version_id"] = v.ID.String()
	payload["brief_id"] = v.BriefID.String()
	if err := p.DB.AuditLog.Create().SetTenantID(v.TenantID).SetScope("tenant").SetEvent(event).SetActor(actor).
		SetSubjectType("creative_version").SetSubjectID(v.ID).SetPayload(payload).Exec(ctx); err != nil {
		p.logger.Warn("autopilot audit not written", zap.String("event", event), zap.Error(err))
	}
}

// trace adds a step to the run's AgentRun trace (trace.veto_window), so the run shows what came
// after its draft.
func (p *Pipeline) trace(ctx context.Context, runID string, entry map[string]any) {
	id, err := uuid.Parse(runID)
	if err != nil {
		return
	}
	run, err := p.DB.AgentRun.Get(ctx, id)
	if err != nil {
		return
	}
	trace := map[string]any{}
	for k, val := range run.Trace {
		trace[k] = val
	}
	if _, ok := entry["at"]; !ok {
		entry["at"] = p.now().UTC().Format(time.RFC3339)
	}
	trace["veto_window"] = withStep(map[string]any{"steps": trace["veto_window"]}, entry)
	if err := p.DB.AgentRun.UpdateOneID(id).SetTrace(trace).Exec(ctx); err != nil {
		p.logger.Warn("autopilot run trace not written", zap.String("run_id", runID), zap.Error(err))
	}
}

// Run sweeps the veto-window videos until ctx ends: a report the Renderer could not hand on,
// a render that waited for the daily cap, and windows that ended.
func (p *Pipeline) Run(ctx context.Context) {
	t := time.NewTicker(p.Config.PipelineInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Sweep(ctx)
		}
	}
}

// sweepBatch bounds one sweep.
const sweepBatch = 50

// Sweep advances every veto-window video that is active and still, or scheduled.
func (p *Pipeline) Sweep(ctx context.Context) {
	ctx = withGateCache(ctx)
	still := p.now().Add(-p.Config.PipelineSettle)
	// Each state has its own batch, so open windows waiting for their end never crowd out videos
	// still being made.
	for _, state := range []string{StateActive, StateScheduled} {
		rows, err := p.DB.CreativeVersion.Query().
			Where(creativeversion.KindEQ(creative.KindVideoAd), creativeversion.UpdatedAtLT(still),
				creativeversion.CreatedAtGT(p.now().AddDate(0, 0, -30)), autopilotStateIn(state)).
			Order(ent.Asc(creativeversion.FieldUpdatedAt)).
			Limit(sweepBatch).
			All(ctx)
		if err != nil {
			p.logger.Warn("autopilot sweep read failed", zap.String("state", state), zap.Error(err))
			continue
		}
		for _, v := range rows {
			if err := p.Advance(ctx, v.TenantID, v.ID); err != nil {
				p.logger.Warn("autopilot sweep step failed", zap.String("version_id", v.ID.String()), zap.Error(err))
			}
		}
	}
}

// autopilotStateIn matches versions whose metadata.autopilot.state is one of states.
func autopilotStateIn(states ...string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString(s.C(creativeversion.FieldMetadata) + "->'" + creative.MetaAutopilot + "'->>'state' IN (")
			for i, st := range states {
				if i > 0 {
					b.WriteString(", ")
				}
				b.Arg(st)
			}
			b.WriteString(")")
		}))
	}
}

// Caption is the post's text: the script's first hook, then the end card's headline and call to
// action. It is the reviewed script's own words, nothing new.
func Caption(v *ent.CreativeVersion, brief *ent.CreativeBrief) string {
	var parts []string
	if script, ok := v.Copy["script"].(map[string]any); ok {
		if hooks, ok := script["hooks"].([]any); ok && len(hooks) > 0 {
			if h, ok := hooks[0].(string); ok && strings.TrimSpace(h) != "" {
				parts = append(parts, strings.TrimSpace(h))
			}
		}
	}
	if card, ok := v.Copy["end_card"].(map[string]any); ok {
		for _, k := range []string{"headline", "cta"} {
			if s, ok := card[k].(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
	}
	if len(parts) == 0 && brief != nil {
		parts = append(parts, brief.Title)
	}
	out := strings.Join(parts, "\n\n")
	// A pharmacy's medicine ad always shows its PPB approval reference (pharmacy hand-off
	// 2026-11-14): the approved version's metadata.ppb_approval.caption_line ends the caption and
	// is never cut by the length limit.
	line := ppbCaptionLine(v)
	limit := 2000
	if line != "" {
		limit -= len([]rune(line)) + 2
	}
	if r := []rune(out); len(r) > limit {
		out = string(r[:limit])
	}
	if line != "" && !strings.Contains(out, line) {
		if out != "" {
			out += "\n\n"
		}
		out += line
	}
	return out
}

// ppbCaptionLine is the version's "PPB approval ref. ..." line, or "".
func ppbCaptionLine(v *ent.CreativeVersion) string {
	if v == nil {
		return ""
	}
	ppb, _ := v.Metadata[creative.MetaPPBApproval].(map[string]any)
	s, _ := ppb["caption_line"].(string)
	return strings.TrimSpace(s)
}

func skippedText(skipped map[string]string) string {
	if len(skipped) == 0 {
		return "no account is chosen"
	}
	parts := make([]string, 0, len(skipped))
	for name, why := range skipped {
		parts = append(parts, name+": "+why)
	}
	return strings.Join(parts, "; ")
}

func parseIDs(raw []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func ignoreConflict(err error) error {
	if errors.Is(err, creative.ErrConflict) {
		return nil
	}
	return err
}

func intOf(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	}
	return 0
}
