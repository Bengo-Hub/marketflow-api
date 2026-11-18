package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/bengobox/marketflow-api/internal/modules/adsmanager"
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
	// adFunds reads the agent's Meta ad account before a paid run; nil skips every paid run,
	// because a paid run is never started without knowing the ad can be funded.
	adFunds AdFundsCheck
}

// AdFundsCheck quotes one ad's budget over its period on the agent's Meta ad account.
type AdFundsCheck func(ctx context.Context, tenantID uuid.UUID, connIDs []string, budget int64, days int) (adsmanager.AccountFunds, error)

// WithAdFunds turns on the ad-account balance check for paid runs (Sprint 20).
func (r *AutopilotRunner) WithAdFunds(f AdFundsCheck) *AutopilotRunner {
	r.adFunds = f
	return r
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

// WithVideoFeatures turns on the video plan gate (creative plan_gate.go) for the runner's videos:
// a run whose video kind the plan does not include is skipped before its brief is made.
func (r *AutopilotRunner) WithVideoFeatures(has func(ctx context.Context, tenantID, feature string) bool) *AutopilotRunner {
	r.creative = r.creative.WithFeatureCheck(has)
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

// errAutopilotNoFunds is a paid run skipped because the ad account cannot fund its next ad.
var errAutopilotNoFunds = fmt.Errorf("%w: the ad account cannot fund the next ad", ErrAutopilotSkipped)

// errAutopilotAdCeiling is a paid run skipped because its next ad would pass the agent's monthly
// ad ceiling on top of this month's real spend.
var errAutopilotAdCeiling = fmt.Errorf("%w: the next ad would pass the monthly ad ceiling", ErrAutopilotSkipped)

// errAutopilotEnded is a run after the agent's end date.
var errAutopilotEnded = fmt.Errorf("%w: the agent has ended", ErrAutopilotSkipped)

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
	if cfg.Ended(r.now(), tenantLocation(ctx, r.db, a.TenantID)) {
		// The end date was its last day: nothing more is made. The agent stays as it is, so a
		// later end date in its settings carries on from there.
		return AutopilotResult{}, fmt.Errorf("%w: its end date of %s has passed", errAutopilotEnded, cfg.EndDate)
	}
	used, err := r.creditsThisMonth(ctx, a)
	if err != nil {
		return AutopilotResult{}, err
	}
	if used >= cfg.CreditCap {
		return AutopilotResult{}, fmt.Errorf("%w: this month's credit ceiling of %d is reached", ErrAutopilotSkipped, cfg.CreditCap)
	}
	var funds *adsmanager.AccountFunds
	var adSpent int64
	if cfg.Paid {
		// Both checked before the credit forecast, so a run whose ad cannot run spends nothing:
		// first the monthly ad ceiling against this month's real spend (our own records), then the
		// ad account on Meta.
		adSpent, err = r.checkAdCeiling(ctx, a, cfg)
		if err != nil {
			return AutopilotResult{}, err
		}
		f, err := r.checkAdFunds(ctx, a, cfg)
		if err != nil {
			return AutopilotResult{}, err
		}
		funds = &f
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
	if funds != nil {
		// The ad account and its funding as read for this run, for the boost that follows.
		b.Metadata["ad_account_connected_id"] = funds.ConnectedID
		b.Metadata["ad_funding"] = string(funds.Quote.Funding)
		b.Metadata["ad_spent_this_month"] = adSpent
	}
	if cfg.Approval == custom_agents.AutoPublish && !vetoMode {
		// This run is approve-each: the brief says so, so the Studio and the reports agree.
		b.Metadata["approval_mode"] = custom_agents.ApproveEach
		b.Metadata["veto_fallback"] = heldWhy
	}
	// Gate G0: a plan without this kind of video skips the run before anything is made.
	if feature := creative.VideoFeature(creative.KindVideoAd, b.Tier); feature != "" {
		if err := r.creative.CheckVideoFeature(ctx, a.TenantID, feature); err != nil {
			return AutopilotResult{}, fmt.Errorf("%w: %v", ErrAutopilotSkipped, err)
		}
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

// checkAdFunds reads the paid agent's Meta ad account live (the Sprint 18 quote) for one ad's
// budget and period. Short funds, a blocked account or no usable ad account skip the run with one
// notice a week; Meta not answering skips it quietly. Every skip retries on the next schedule.
func (r *AutopilotRunner) checkAdFunds(ctx context.Context, a *ent.CustomAgent, cfg custom_agents.AutopilotConfig) (adsmanager.AccountFunds, error) {
	if r.adFunds == nil {
		return adsmanager.AccountFunds{}, fmt.Errorf("%w: the ad account check is not available", errAutopilotNoFunds)
	}
	f, err := r.adFunds(ctx, a.TenantID, cfg.AccountIDs, cfg.PerAdBudget, cfg.PeriodDays)
	switch {
	case errors.Is(err, adsmanager.ErrNoAdAccount), errors.Is(err, adsmanager.ErrAdAccountToken):
		r.tellNoFunds(ctx, a, err.Error(), "")
		return f, fmt.Errorf("%w: %s", errAutopilotNoFunds, err.Error())
	case err != nil:
		r.logger.Warn("autopilot ad account check unavailable", zap.String("agent", a.ID.String()), zap.Error(err))
		return f, fmt.Errorf("%w: Meta did not answer about the ad account; it tries again on its next schedule", errAutopilotNoFunds)
	}
	if ok, why := f.PaidRunReady(); !ok {
		r.tellNoFunds(ctx, a, why, f.Quote.AccountID)
		return f, fmt.Errorf("%w: %s; it runs again on its next schedule", errAutopilotNoFunds, why)
	}
	return f, nil
}

// tellNoFunds emails the tenant's admins why a paid run waited, with Meta's billing page (Add
// funds with M-Pesa) when the ad account is known, else the portal's connections page. Once per
// agent per ISO week, like the credits notice.
func (r *AutopilotRunner) tellNoFunds(ctx context.Context, a *ent.CustomAgent, why, adAccountID string) {
	action := "Connect your Meta ad account here: " + r.portalLink(ctx, a, "/settings/integrations")
	if adAccountID != "" {
		action = "Add funds with M-Pesa on Meta: " + adsmanager.BillingURL(adAccountID)
	}
	year, week := r.now().UTC().ISOWeek()
	body := fmt.Sprintf("Your agent \"%s\" makes paid ads, and it skipped this run because %s. Nothing was made or charged, and it checks the ad account again on its next schedule.\n\n%s",
		a.Name, why, action)
	r.tellPaidSkip(ctx, a, "Your ad agent is waiting for your ad account", body, "autopilot_no_ad_funds",
		fmt.Sprintf("autopilot-ad-funds-%s-%d-%d", a.ID, year, week))
}

// checkAdCeiling reads this month's real spend of the agent's boosts (the stored ad metrics) and
// skips the run when one more ad of per_ad_budget would pass monthly_ad_ceiling, with one email
// per agent per month: the ceiling holds until the month ends or the owner raises it. Spend that
// cannot be read skips the run quietly, because a money cap is never passed on a guess; it tries
// again on its next schedule.
func (r *AutopilotRunner) checkAdCeiling(ctx context.Context, a *ent.CustomAgent, cfg custom_agents.AutopilotConfig) (int64, error) {
	ok, why, spent, err := custom_agents.AdCeilingCheck(ctx, r.db, a, cfg, cfg.PerAdBudget, r.now())
	if err != nil {
		r.logger.Warn("autopilot ad spend unreadable", zap.String("agent", a.ID.String()), zap.Error(err))
		return 0, fmt.Errorf("%w: this month's ad spend could not be read; it tries again on its next schedule", errAutopilotAdCeiling)
	}
	if ok {
		return spent, nil
	}
	month := r.now().UTC().Format("2006-01")
	body := fmt.Sprintf("Your agent \"%s\" makes paid ads, and it skipped this run because %s. Nothing was made or charged. "+
		"It makes paid ads again next month, or sooner if you raise the monthly ad ceiling in its settings.\n\nSettings: %s",
		a.Name, why, r.portalLink(ctx, a, "/studio/autopilot"))
	r.tellPaidSkip(ctx, a, "Your ad agent reached its monthly ad ceiling", body, "autopilot_ad_ceiling",
		fmt.Sprintf("autopilot-ad-ceiling-%s-%s", a.ID, month))
	return spent, fmt.Errorf("%w: %s; it runs again on its next schedule", errAutopilotAdCeiling, why)
}

// tellPaidSkip emails the tenant's admins why a paid run was skipped; idem (plus the address)
// keeps it to one email per period.
func (r *AutopilotRunner) tellPaidSkip(ctx context.Context, a *ent.CustomAgent, subject, body, kind, idem string) {
	if r.notif == nil {
		return
	}
	emails, err := tenantAdminEmails(ctx, r.db, a.TenantID, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	for _, email := range emails {
		if err := r.notif.Send(ctx, a.TenantID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: subject, Body: body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": kind},
			IdempotencyKey: idem + "-" + email,
		}); err != nil {
			r.logger.Warn("autopilot paid skip notice not sent", zap.String("agent", a.ID.String()), zap.String("kind", kind), zap.Error(err))
		}
	}
}

// AdFunds is the live check through the agent's own ad-account token, with the platform's
// appsecret_proof like every other Meta call.
func AdFunds(db *ent.Client, keys *encryption.KeyProvider) AdFundsCheck {
	m := meta.NewClient("").WithSecret(meta.PlatformSecret(db, keys, os.Getenv("MF_META_APP_SECRET")))
	return func(ctx context.Context, tenantID uuid.UUID, connIDs []string, budget int64, days int) (adsmanager.AccountFunds, error) {
		return adsmanager.CheckFunds(ctx, db, keys, m, tenantID, connIDs, budget, days)
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
	case errors.Is(err, errAutopilotNoFunds):
		upd.SetStatus("skipped").SetErrorMessage(truncateStr(err.Error(), 1000)).SetTrace(map[string]any{"skip": "ad_funds"})
	case errors.Is(err, errAutopilotAdCeiling):
		upd.SetStatus("skipped").SetErrorMessage(truncateStr(err.Error(), 1000)).SetTrace(map[string]any{"skip": "ad_ceiling"})
	case errors.Is(err, errAutopilotEnded):
		upd.SetStatus("skipped").SetErrorMessage(truncateStr(err.Error(), 1000)).SetTrace(map[string]any{"skip": "ended"})
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
