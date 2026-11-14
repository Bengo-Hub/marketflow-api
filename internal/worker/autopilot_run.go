package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/modules/funnels"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AutopilotRunner runs an ad_autopilot agent (Sprint 20) in deterministic code: the config picks
// the offering, language and tier; the creative module then writes the script with its normal
// gates and charges. In approve-each mode a run ends at a draft for the owner to review. In the
// veto-window mode (auto_publish, owner 2026-11-14) the version is marked for the autopublish
// pipeline, which carries it on to an organic post after the window unless the owner vetoes it.
// A run never spends on ads.
type AutopilotRunner struct {
	db       *ent.Client
	creative *creative.Service
	notif    *notifications.Client
	appURL   string
	logger   *zap.Logger
	now      func() time.Time
	forecast func(ctx context.Context, tenantID uuid.UUID) (custom_agents.CreditForecast, error)
	// gate re-checks the plan feature and the terms at each run; the zero gate allows nothing, so
	// an auto_publish agent's run falls back to approve-each.
	gate custom_agents.AutoPublishGate
	// lapsed is told when the gate fails for an auto_publish agent, so the tenant's posts still
	// waiting in a veto window are cancelled at once; nil leaves that to the pipeline's sweep.
	lapsed *AutopilotLapsedHook
}

// WithAutoPublishGate lets veto-window runs go ahead when the plan and terms still allow them.
func (r *AutopilotRunner) WithAutoPublishGate(g custom_agents.AutoPublishGate) *AutopilotRunner {
	r.gate = g
	return r
}

// WithLapsedHook cancels the tenant's waiting veto-window posts when a run finds the plan or the
// terms no longer allow the mode (owner 2026-11-14).
func (r *AutopilotRunner) WithLapsedHook(h *AutopilotLapsedHook) *AutopilotRunner {
	r.lapsed = h
	return r
}

func NewAutopilotRunner(db *ent.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *AutopilotRunner {
	// Ads the autopilot makes get their lead page too, the same as ads made in the portal.
	svc := creative.NewService(db, logger).WithLeadFunnels(funnels.NewService(db, logger).ForAds())
	return &AutopilotRunner{db: db, creative: svc, notif: notif, appURL: appURL, logger: logger.Named("autopilot"), now: time.Now}
}

// ErrAutopilotSkipped is a run that had nothing wrong but must not go ahead (ceiling reached).
var ErrAutopilotSkipped = errors.New("autopilot run skipped")

// errAutopilotNoCredits is a skip the owner can fix by buying credits.
var errAutopilotNoCredits = fmt.Errorf("%w: not enough AI credits", ErrAutopilotSkipped)

// WithForecast checks each run's script price against the balance. Nothing is spent.
func (r *AutopilotRunner) WithForecast(f custom_agents.CreditForecaster) *AutopilotRunner {
	if f != nil {
		r.forecast = func(ctx context.Context, tenantID uuid.UUID) (custom_agents.CreditForecast, error) {
			return f(ctx, tenantID, custom_agents.OutputScript)
		}
	}
	return r
}

// AutopilotResult is what the run made.
type AutopilotResult struct {
	BriefID   string `json:"brief_id"`
	VersionID string `json:"version_id"`
	Offering  string `json:"offering"`
	Run       int    `json:"autopilot_run"`
	// VetoWindow is true when the draft carries on by itself to a post after the veto window.
	VetoWindow bool `json:"veto_window,omitempty"`
}

// Run makes this run's brief and script draft. The rotation counts the agent's earlier finished
// runs, so a failed run retries the same offering.
func (r *AutopilotRunner) Run(ctx context.Context, a *ent.CustomAgent) (AutopilotResult, error) {
	return r.run(ctx, a, nil, uuid.Nil)
}

// RunWithID is Run for a run already recorded as an AgentRun: the brief and the veto-window steps
// carry the run id, so every later step is traced to it.
func (r *AutopilotRunner) RunWithID(ctx context.Context, a *ent.CustomAgent, runID uuid.UUID) (AutopilotResult, error) {
	return r.run(ctx, a, nil, runID)
}

// RunForItem drafts an ad for one new catalogue item instead of the next rotated offering, through
// the same ceiling and credit checks.
func (r *AutopilotRunner) RunForItem(ctx context.Context, a *ent.CustomAgent, offering map[string]any, runID uuid.UUID) (AutopilotResult, error) {
	return r.run(ctx, a, offering, runID)
}

func (r *AutopilotRunner) run(ctx context.Context, a *ent.CustomAgent, only map[string]any, runID uuid.UUID) (AutopilotResult, error) {
	cfg, offerings, err := custom_agents.ReadAutopilot(a)
	if err != nil {
		return AutopilotResult{}, err
	}
	// The saved config passed the gate when it was saved; the plan or the terms may have changed
	// since, so a veto-window run checks again and falls back to approve-each when they no longer
	// allow it (heldWhy says why, for the email).
	vetoMode, heldWhy := false, ""
	if cfg.Approval == custom_agents.AutoPublish {
		vetoMode, heldWhy = r.gate.Allowed(ctx, a.TenantID)
		if !vetoMode {
			// Posts already waiting in a window are cancelled now, not only new runs held: the
			// pipeline checks the plan and terms itself before it cancels, and emails the owner.
			r.lapsed.Call(ctx, a.TenantID)
		}
	}
	if err := cfg.Validate(cfg.Approval == custom_agents.AutoPublish); err != nil {
		return AutopilotResult{}, err
	}
	used, err := r.creditsThisMonth(ctx, a)
	if err != nil {
		return AutopilotResult{}, err
	}
	if used >= cfg.CreditCap {
		return AutopilotResult{}, fmt.Errorf("%w: this month's credit ceiling of %d is reached", ErrAutopilotSkipped, cfg.CreditCap)
	}
	if r.forecast != nil {
		// A forecast that cannot be read never blocks a run: the script's own charge still refuses
		// a spend that does not fit.
		fc, err := r.forecast(ctx, a.TenantID)
		switch {
		case err != nil:
			r.logger.Warn("autopilot credit forecast unavailable", zap.String("agent", a.ID.String()), zap.Error(err))
		case !cfg.CreditsFit(used, fc.CreditsNeeded):
			return AutopilotResult{}, fmt.Errorf("%w: this draft needs %d credits and would pass this month's ceiling of %d", ErrAutopilotSkipped, fc.CreditsNeeded, cfg.CreditCap)
		case !fc.Enough:
			r.tellShort(ctx, a, fc)
			return AutopilotResult{}, fmt.Errorf("%w: this draft needs %d credits and the balance is %d; it runs again on its next schedule", errAutopilotNoCredits, fc.CreditsNeeded, fc.Balance)
		}
	}
	if only != nil {
		id, _ := only["source_id"].(string)
		cfg.OfferingIDs, offerings = []string{id}, []map[string]any{only}
	}
	n, err := custom_agents.NextRunNumber(ctx, r.db, a.ID)
	if err != nil {
		return AutopilotResult{}, err
	}
	b, err := custom_agents.NextBrief(a.ID.String(), cfg, offerings, n, r.now())
	if err != nil {
		return AutopilotResult{}, err
	}
	if runID != uuid.Nil {
		b.Metadata["autopilot_run_id"] = runID.String()
	}
	if cfg.Approval == custom_agents.AutoPublish && !vetoMode {
		// This run is approve-each: the brief says so, so the Studio and the reports agree.
		b.Metadata["approval_mode"] = custom_agents.ApproveEach
		b.Metadata["veto_fallback"] = heldWhy
	}
	brief, err := r.creative.CreateBrief(ctx, a.TenantID, nil, creative.BriefInput{
		Title: &b.Title, Offering: b.Offering, Objective: &b.Objective, Language: &b.Language, Metadata: b.Metadata,
	})
	if err != nil {
		return AutopilotResult{}, fmt.Errorf("brief: %w", err)
	}
	in := creative.VideoAdInput{Kind: b.Tier, Language: b.Language}
	if vetoMode {
		in.Autopilot = VetoStart(a.ID, runID, n, cfg.VetoHours, r.now())
	}
	version, err := r.creative.CreateVideoAd(ctx, a.TenantID, brief.ID, in)
	if err != nil {
		return AutopilotResult{}, fmt.Errorf("script: %w", err)
	}
	name, _ := b.Offering["name"].(string)
	veto := 0
	if vetoMode {
		veto = cfg.VetoHours
	}
	r.tell(ctx, a, name, brief.ID.String(), veto, heldWhy)
	return AutopilotResult{BriefID: brief.ID.String(), VersionID: version.ID.String(), Offering: name, Run: n, VetoWindow: vetoMode}, nil
}

// VetoStart is a veto-window video's first metadata.autopilot: the agent and run that own it and
// the first step, taken by the agent.
func VetoStart(agentID, runID uuid.UUID, run, vetoHours int, now time.Time) map[string]any {
	ap := map[string]any{
		"agent_id": agentID.String(), "run": run, "mode": "veto_window", "state": "active", "veto_hours": vetoHours,
		"steps": []any{map[string]any{"step": "script_requested", "by": "agent:" + agentID.String(), "at": now.UTC().Format(time.RFC3339)}},
	}
	if runID != uuid.Nil {
		ap["run_id"] = runID.String()
	}
	return ap
}

// creditsThisMonth sums what this agent's drafts really cost since the 1st.
func (r *AutopilotRunner) creditsThisMonth(ctx context.Context, a *ent.CustomAgent) (int, error) {
	return custom_agents.CreditsThisMonth(ctx, r.db, a.TenantID, a.ID, r.now())
}

// tell emails the tenant's admins that a draft waits for their review, or, in veto-window mode,
// that it carries on and a heads-up comes before anything is posted.
func (r *AutopilotRunner) tell(ctx context.Context, a *ent.CustomAgent, offering, briefID string, vetoHours int, heldWhy string) {
	if r.notif == nil {
		return
	}
	emails, err := tenantAdminEmails(ctx, r.db, a.TenantID, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	link := r.portalLink(ctx, a, "/studio/"+briefID)
	body := fmt.Sprintf("Your agent \"%s\" drafted a new ad for %s. Nothing is posted or paid for until you approve it.\n\nReview it here: %s", a.Name, offering, link)
	if vetoHours > 0 {
		body = fmt.Sprintf("Your agent \"%s\" drafted a new ad for %s. It now makes the storyboard and the video by itself, within your monthly credit ceiling. "+
			"Before anything is posted you get another email saying what will be posted, where and when, with a link to cancel it. "+
			"The post goes out %d hours after that email unless you cancel it. Paid boosts always wait for your approval.\n\nFollow it here: %s",
			a.Name, offering, vetoHours, link)
	} else if heldWhy != "" {
		body = fmt.Sprintf("Your agent \"%s\" drafted a new ad for %s. It is set to post after a veto window, but this one waits for your approval because %s. Nothing is posted or paid for until you approve it.\n\nReview it here: %s",
			a.Name, offering, heldWhy, link)
	}
	for _, email := range emails {
		if err := r.notif.Send(ctx, a.TenantID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: "A new ad draft is ready for your review", Body: body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "autopilot_draft"},
			IdempotencyKey: "autopilot-draft-" + briefID + "-" + email,
		}); err != nil {
			r.logger.Warn("autopilot notice not sent", zap.String("agent", a.ID.String()), zap.Error(err))
		}
	}
}

// portalLink builds a link into the tenant's portal.
func (r *AutopilotRunner) portalLink(ctx context.Context, a *ent.CustomAgent, path string) string {
	if t, err := r.db.Tenant.Get(ctx, a.TenantID); err == nil && t.Slug != "" {
		return r.appURL + "/" + t.Slug + path
	}
	return r.appURL + path
}

// tellShort emails the tenant's admins a Buy credits link, once per agent per ISO week, so a daily
// agent on an empty balance does not send one email a day.
func (r *AutopilotRunner) tellShort(ctx context.Context, a *ent.CustomAgent, fc custom_agents.CreditForecast) {
	if r.notif == nil {
		return
	}
	emails, err := tenantAdminEmails(ctx, r.db, a.TenantID, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	year, week := r.now().UTC().ISOWeek()
	body := fmt.Sprintf("Your agent \"%s\" skipped its draft because your AI credits are low: it needs %d and your balance is %d. Nothing was charged, and it tries again on its next schedule.\n\nBuy credits here: %s",
		a.Name, fc.CreditsNeeded, fc.Balance, r.portalLink(ctx, a, "/settings/billing"))
	for _, email := range emails {
		if err := r.notif.Send(ctx, a.TenantID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: "Your ad agent needs more AI credits", Body: body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "autopilot_no_credits"},
			IdempotencyKey: fmt.Sprintf("autopilot-credits-%s-%d-%d-%s", a.ID, year, week, email),
		}); err != nil {
			r.logger.Warn("autopilot credits notice not sent", zap.String("agent", a.ID.String()), zap.Error(err))
		}
	}
}

// runAutopilot makes a scheduled or button run and records its outcome.
func (c *CustomAgentExecutionConsumer) runAutopilot(ctx context.Context, a *ent.CustomAgent, runID uuid.UUID) {
	res, err := c.autopilot.RunWithID(ctx, a, runID)
	c.autopilot.record(ctx, a, runID, res, err)
}

// record stores a run's outcome. A reached ceiling or a short balance is "skipped", not a failure:
// the agent tries again on its next schedule.
func (r *AutopilotRunner) record(ctx context.Context, a *ent.CustomAgent, runID uuid.UUID, res AutopilotResult, err error) {
	upd := r.db.AgentRun.UpdateOneID(runID).SetFinishedAt(time.Now())
	switch {
	case errors.Is(err, errAutopilotNoCredits):
		upd.SetStatus("skipped").SetErrorMessage(truncateStr(err.Error(), 1000)).SetTrace(map[string]any{"skip": "credits"})
	case errors.Is(err, ErrAutopilotSkipped):
		upd.SetStatus("skipped").SetErrorMessage(truncateStr(err.Error(), 1000))
	case err != nil:
		r.logger.Warn("autopilot run failed", zap.String("run_id", runID.String()), zap.Error(err))
		upd.SetStatus("failed").SetErrorMessage(truncateStr(err.Error(), 1000))
	default:
		upd.SetStatus("completed").SetTrace(map[string]any{"autopilot": res})
		_, _ = r.db.CustomAgent.UpdateOneID(a.ID).AddRunCount(1).SetLastRunAt(time.Now()).Save(ctx)
	}
	if _, err := upd.Save(ctx); err != nil {
		r.logger.Error("autopilot run: record outcome", zap.String("run_id", runID.String()), zap.Error(err))
	}
}
