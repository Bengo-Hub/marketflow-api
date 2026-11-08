package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AutopilotRunner runs an ad_autopilot agent (Sprint 20) in deterministic code: the config picks
// the offering, language and tier; the creative module then writes the script with its normal
// gates and charges. A run ends at a draft for the owner to review. It never approves, posts or
// spends: an agent can never approve its own work.
type AutopilotRunner struct {
	db       *ent.Client
	creative *creative.Service
	notif    *notifications.Client
	appURL   string
	logger   *zap.Logger
	now      func() time.Time
	forecast func(ctx context.Context, tenantID uuid.UUID) (custom_agents.CreditForecast, error)
}

func NewAutopilotRunner(db *ent.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *AutopilotRunner {
	return &AutopilotRunner{db: db, creative: creative.NewService(db, logger), notif: notif, appURL: appURL, logger: logger.Named("autopilot"), now: time.Now}
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
}

// Run makes this run's brief and script draft. The rotation counts the agent's earlier finished
// runs, so a failed run retries the same offering.
func (r *AutopilotRunner) Run(ctx context.Context, a *ent.CustomAgent) (AutopilotResult, error) {
	return r.run(ctx, a, nil)
}

// RunForItem drafts an ad for one new catalogue item instead of the next rotated offering, through
// the same ceiling and credit checks.
func (r *AutopilotRunner) RunForItem(ctx context.Context, a *ent.CustomAgent, offering map[string]any) (AutopilotResult, error) {
	return r.run(ctx, a, offering)
}

func (r *AutopilotRunner) run(ctx context.Context, a *ent.CustomAgent, only map[string]any) (AutopilotResult, error) {
	cfg, offerings, err := custom_agents.ReadAutopilot(a)
	if err != nil {
		return AutopilotResult{}, err
	}
	// Auto-publish is not honoured here yet: every run ends at a draft, whatever the mode.
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
	brief, err := r.creative.CreateBrief(ctx, a.TenantID, nil, creative.BriefInput{
		Title: &b.Title, Offering: b.Offering, Objective: &b.Objective, Language: &b.Language, Metadata: b.Metadata,
	})
	if err != nil {
		return AutopilotResult{}, fmt.Errorf("brief: %w", err)
	}
	version, err := r.creative.CreateVideoAd(ctx, a.TenantID, brief.ID, creative.VideoAdInput{Kind: b.Tier, Language: b.Language})
	if err != nil {
		return AutopilotResult{}, fmt.Errorf("script: %w", err)
	}
	name, _ := b.Offering["name"].(string)
	r.tell(ctx, a, name, brief.ID.String())
	return AutopilotResult{BriefID: brief.ID.String(), VersionID: version.ID.String(), Offering: name, Run: n}, nil
}

// creditsThisMonth sums what this agent's drafts really cost since the 1st, from the versions of
// its briefs (the tenant's status and created_at index keeps the brief read small).
func (r *AutopilotRunner) creditsThisMonth(ctx context.Context, a *ent.CustomAgent) (int, error) {
	now := r.now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	ids, err := r.db.CreativeBrief.Query().
		Where(creativebrief.TenantID(a.TenantID), creativebrief.CreatedAtGTE(start), func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(s.C(creativebrief.FieldMetadata) + "->>'autopilot_agent_id' = ").Arg(a.ID.String())
			}))
		}).IDs(ctx)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	var totals []struct {
		Sum int `json:"sum"`
	}
	if err := r.db.CreativeVersion.Query().Where(creativeversion.BriefIDIn(ids...)).
		Aggregate(ent.Sum(creativeversion.FieldCredits)).Scan(ctx, &totals); err != nil || len(totals) == 0 {
		return 0, err
	}
	return totals[0].Sum, nil
}

// tell emails the tenant's admins that a draft waits for their review.
func (r *AutopilotRunner) tell(ctx context.Context, a *ent.CustomAgent, offering, briefID string) {
	if r.notif == nil {
		return
	}
	emails, err := tenantAdminEmails(ctx, r.db, a.TenantID, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	link := r.portalLink(ctx, a, "/studio/"+briefID)
	body := fmt.Sprintf("Your agent \"%s\" drafted a new ad for %s. Nothing is posted or paid for until you approve it.\n\nReview it here: %s", a.Name, offering, link)
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
	res, err := c.autopilot.Run(ctx, a)
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
