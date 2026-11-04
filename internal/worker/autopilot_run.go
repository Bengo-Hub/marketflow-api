package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	forecast func(ctx context.Context, tenantID uuid.UUID) (creditForecast, error)
}

func NewAutopilotRunner(db *ent.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *AutopilotRunner {
	return &AutopilotRunner{db: db, creative: creative.NewService(db, logger), notif: notif, appURL: appURL, logger: logger.Named("autopilot"), now: time.Now}
}

// ErrAutopilotSkipped is a run that had nothing wrong but must not go ahead (ceiling reached).
var ErrAutopilotSkipped = errors.New("autopilot run skipped")

// errAutopilotNoCredits is a skip the owner can fix by buying credits.
var errAutopilotNoCredits = fmt.Errorf("%w: not enough AI credits", ErrAutopilotSkipped)

// creditForecast is marketflow-ai's answer to what a draft costs now. Balance is -1 when it is
// unlimited or could not be read.
type creditForecast struct {
	CreditsNeeded int  `json:"credits_needed"`
	Balance       int  `json:"balance"`
	Enough        bool `json:"enough"`
}

// WithForecast checks each run's script price against the balance through marketflow-ai, the one
// owner of the subscriptions-api credit client. Nothing is spent.
func (r *AutopilotRunner) WithForecast(aiURL, key string) *AutopilotRunner {
	if aiURL == "" || key == "" {
		return r
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	url := strings.TrimSuffix(aiURL, "/") + "/api/v1/internal/credits/forecast"
	r.forecast = func(ctx context.Context, tenantID uuid.UUID) (creditForecast, error) {
		var fc creditForecast
		body, _ := json.Marshal(map[string]string{"tenant_id": tenantID.String(), "output": "script"})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fc, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", key)
		resp, err := hc.Do(req)
		if err != nil {
			return fc, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fc, fmt.Errorf("credit forecast: status %d", resp.StatusCode)
		}
		return fc, json.NewDecoder(resp.Body).Decode(&fc)
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
func (r *AutopilotRunner) tellShort(ctx context.Context, a *ent.CustomAgent, fc creditForecast) {
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

// runAutopilot records the run's outcome. A reached ceiling or a short balance is "skipped", not
// a failure: the agent tries again on its next schedule.
func (c *CustomAgentExecutionConsumer) runAutopilot(ctx context.Context, a *ent.CustomAgent, runID uuid.UUID) {
	res, err := c.autopilot.Run(ctx, a)
	upd := c.db.AgentRun.UpdateOneID(runID).SetFinishedAt(time.Now())
	switch {
	case errors.Is(err, errAutopilotNoCredits):
		upd.SetStatus("skipped").SetErrorMessage(truncateStr(err.Error(), 1000)).SetTrace(map[string]any{"skip": "credits"})
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
