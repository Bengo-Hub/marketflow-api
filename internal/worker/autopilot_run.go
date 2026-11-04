package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
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
}

func NewAutopilotRunner(db *ent.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *AutopilotRunner {
	return &AutopilotRunner{db: db, creative: creative.NewService(db, logger), notif: notif, appURL: appURL, logger: logger.Named("autopilot"), now: time.Now}
}

// ErrAutopilotSkipped is a run that had nothing wrong but must not go ahead (ceiling reached).
var ErrAutopilotSkipped = errors.New("autopilot run skipped")

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
	var cfg custom_agents.AutopilotConfig
	raw, err := json.Marshal(a.Metadata["autopilot"])
	if err != nil {
		return AutopilotResult{}, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return AutopilotResult{}, fmt.Errorf("the autopilot settings could not be read: %w", err)
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
	var offerings []map[string]any
	if raw, err := json.Marshal(a.Metadata["autopilot_offerings"]); err == nil {
		_ = json.Unmarshal(raw, &offerings)
	}
	n, err := r.db.AgentRun.Query().Where(agentrun.CustomAgentID(a.ID), agentrun.StatusEQ("completed")).Count(ctx)
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
	link := r.appURL + "/studio/" + briefID
	if t, err := r.db.Tenant.Get(ctx, a.TenantID); err == nil && t.Slug != "" {
		link = r.appURL + "/" + t.Slug + "/studio/" + briefID
	}
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

// runAutopilot records the run's outcome. A reached ceiling is "skipped", not a failure: the
// agent tries again on its next schedule.
func (c *CustomAgentExecutionConsumer) runAutopilot(ctx context.Context, a *ent.CustomAgent, runID uuid.UUID) {
	res, err := c.autopilot.Run(ctx, a)
	upd := c.db.AgentRun.UpdateOneID(runID).SetFinishedAt(time.Now())
	switch {
	case errors.Is(err, ErrAutopilotSkipped):
		upd.SetStatus("skipped").SetErrorMessage(truncateStr(err.Error(), 1000))
	case err != nil:
		c.logger.Warn("autopilot run failed", zap.String("run_id", runID.String()), zap.Error(err))
		upd.SetStatus("failed").SetErrorMessage(truncateStr(err.Error(), 1000))
	default:
		upd.SetStatus("completed").SetTrace(map[string]any{"autopilot": res})
		_, _ = c.db.CustomAgent.UpdateOneID(a.ID).AddRunCount(1).SetLastRunAt(time.Now()).Save(ctx)
	}
	if _, err := upd.Save(ctx); err != nil {
		c.logger.Error("autopilot run: record outcome", zap.String("run_id", runID.String()), zap.Error(err))
	}
}
