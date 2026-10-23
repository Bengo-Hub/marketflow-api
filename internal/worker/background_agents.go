package worker

// background_agents.go contains Vera's autonomous background agents that run
// on cron schedules or NATS event triggers within the marketflow-worker.
//
// Cron agents (ProactiveFollowUpAgent, DealStaleDetector) run as goroutines
// via CronAgentRunner rather than NATS consumers since they are tenant-fan-out
// loops. Event-triggered agents (LeadScorerAgent, NurtureSequenceEnhancer) run
// as standard NATS Handler implementations registered in NewRegistry.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	sharedcache "github.com/Bengo-Hub/cache"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// agentS2SPost is a small helper for JSON POST calls with X-API-Key auth.
func agentS2SPost(ctx context.Context, hc *http.Client, url, serviceKey string, body map[string]any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", serviceKey)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("s2s %s → %d: %s", url, resp.StatusCode, truncateStr(string(rb), 200))
	}
	return nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── LeadScorerAgent ─────────────────────────────────────────────────────────

// LeadScorerAgent listens to lead.created (published by the leads service outbox) and
// crm.activity.logged.
// It calls the lead rescore endpoint and, if the score crosses 70, publishes
// crm.lead.qualified back to NATS.
type LeadScorerAgent struct {
	db         *ent.Client
	apiBaseURL string
	serviceKey string
	js         jetstream.JetStream
	logger     *zap.Logger
	hc         *http.Client
}

// Subject was "crm.lead.created", which nothing ever published, so the scorer never ran for new
// leads. It now follows the subject the leads outbox actually emits.
func (a *LeadScorerAgent) Subject() string { return leads.LeadCreatedTopic }
func (a *LeadScorerAgent) Durable() string { return "marketflow-worker-agent-lead-scorer" }

func (a *LeadScorerAgent) Handle(ctx context.Context, msg jetstream.Msg) error {
	ev, err := decodeLeadCreated(msg.Data())
	if err != nil {
		return dropPoison(a.logger, msg, "decode lead.created", err)
	}
	if a.apiBaseURL == "" {
		return nil
	}
	return a.rescore(ctx, ev.TenantID.String(), ev.LeadID.String())
}

func (a *LeadScorerAgent) rescore(ctx context.Context, tenantID, leadID string) error {
	url := strings.TrimSuffix(a.apiBaseURL, "/") + "/api/v1/leads/" + leadID + "/rescore"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", a.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID)
	resp, err := a.hc.Do(req)
	if err != nil {
		a.logger.Warn("lead rescore request failed", zap.String("lead_id", leadID), zap.Error(err))
		return nil // fail-open
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	var result struct {
		Score    int `json:"score"`
		OldScore int `json:"old_score"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return nil
	}

	// Publish qualified event if score just crossed the threshold
	if result.Score >= 70 && result.OldScore < 70 && a.js != nil {
		payload, _ := json.Marshal(map[string]any{
			"tenant_id": tenantID,
			"lead_id":   leadID,
			"score":     result.Score,
		})
		if _, err := a.js.Publish(ctx, "crm.lead.qualified", payload); err != nil {
			a.logger.Warn("publish crm.lead.qualified failed", zap.Error(err))
		} else {
			a.logger.Info("lead qualified — published crm.lead.qualified",
				zap.String("lead_id", leadID), zap.Int("score", result.Score))
		}
	}
	return nil
}

// ActivityLoggedLeadScorerAgent handles crm.activity.logged for lead rescoring.
type ActivityLoggedLeadScorerAgent struct {
	*LeadScorerAgent
}

func (a *ActivityLoggedLeadScorerAgent) Subject() string { return "crm.activity.logged" }
func (a *ActivityLoggedLeadScorerAgent) Durable() string {
	return "marketflow-worker-agent-activity-lead-scorer"
}

func (a *ActivityLoggedLeadScorerAgent) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID   string `json:"tenant_id"`
		EntityType string `json:"entity_type"`
		EntityID   string `json:"entity_id"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return nil
	}
	if ev.EntityType != "lead" {
		return nil
	}
	return a.rescore(ctx, ev.TenantID, ev.EntityID)
}

// ── ProactiveFollowUpAgent ───────────────────────────────────────────────────

// ProactiveFollowUpAgent is a CronHandler (implements Subject() "cron.hourly").
// It queries leads and contacts with no recent activity and creates follow-up tasks.
type ProactiveFollowUpAgent struct {
	db         *ent.Client
	apiBaseURL string
	serviceKey string
	logger     *zap.Logger
	hc         *http.Client
}

func (a *ProactiveFollowUpAgent) Subject() string { return "cron.followup.hourly" }
func (a *ProactiveFollowUpAgent) Durable() string { return "marketflow-worker-agent-followup" }

func (a *ProactiveFollowUpAgent) Handle(ctx context.Context, msg jetstream.Msg) error {
	if a.db == nil || a.apiBaseURL == "" {
		return nil
	}

	var ev struct {
		TenantID     string `json:"tenant_id"`
		FollowUpDays int    `json:"follow_up_days"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return nil
	}
	if ev.FollowUpDays <= 0 {
		ev.FollowUpDays = 7
	}

	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return nil
	}

	cutoff := time.Now().AddDate(0, 0, -ev.FollowUpDays)
	leads, _ := a.db.Lead.Query().
		Where(
			lead.TenantIDEQ(tenantID),
			lead.StatusEQ("active"),
			lead.UpdatedAtLT(cutoff),
		).
		Limit(50).
		All(ctx)

	base := strings.TrimSuffix(a.apiBaseURL, "/")
	for _, l := range leads {
		title := fmt.Sprintf("Follow up — no contact in %d days", ev.FollowUpDays)
		due := time.Now().Add(24 * time.Hour)
		_ = agentS2SPost(ctx, a.hc, base+"/api/v1/tasks", a.serviceKey, map[string]any{
			"entity_type": "lead",
			"entity_id":   l.ID.String(),
			"title":       title,
			"due_at":      due.Format(time.RFC3339),
			"priority":    "normal",
			"tenant_id":   ev.TenantID,
		})
	}

	contacts, _ := a.db.Contact.Query().
		Where(
			contact.TenantIDEQ(tenantID),
			contact.UpdatedAtLT(cutoff),
		).
		Limit(50).
		All(ctx)

	for _, c := range contacts {
		title := fmt.Sprintf("Follow up — no contact in %d days", ev.FollowUpDays)
		due := time.Now().Add(24 * time.Hour)
		_ = agentS2SPost(ctx, a.hc, base+"/api/v1/tasks", a.serviceKey, map[string]any{
			"entity_type": "contact",
			"entity_id":   c.ID.String(),
			"title":       title,
			"due_at":      due.Format(time.RFC3339),
			"priority":    "normal",
			"tenant_id":   ev.TenantID,
		})
	}

	a.logger.Info("proactive followup agent ran",
		zap.String("tenant", ev.TenantID),
		zap.Int("leads_queued", len(leads)),
		zap.Int("contacts_queued", len(contacts)),
	)
	return nil
}

// ── DealStaleDetector ────────────────────────────────────────────────────────

// DealStaleDetector is triggered daily (via NATS cron.daily event).
// It queries open deals not updated in 14+ days and creates stale tasks.
type DealStaleDetector struct {
	apiBaseURL string
	serviceKey string
	logger     *zap.Logger
	hc         *http.Client
}

func (a *DealStaleDetector) Subject() string { return "cron.deal.stale.daily" }
func (a *DealStaleDetector) Durable() string { return "marketflow-worker-agent-deal-stale" }

func (a *DealStaleDetector) Handle(ctx context.Context, msg jetstream.Msg) error {
	if a.apiBaseURL == "" {
		return nil
	}

	var ev struct {
		TenantID  string `json:"tenant_id"`
		StaleDays int    `json:"stale_days"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return nil
	}
	if ev.StaleDays <= 0 {
		ev.StaleDays = 14
	}

	base := strings.TrimSuffix(a.apiBaseURL, "/")
	url := fmt.Sprintf("%s/api/v1/deals?status=open&stale_days=%d&tenant_id=%s&limit=50",
		base, ev.StaleDays, ev.TenantID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-API-Key", a.serviceKey)
	req.Header.Set("X-Tenant-ID", ev.TenantID)
	resp, err := a.hc.Do(req)
	if err != nil || resp.StatusCode >= 300 {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	var result struct {
		Data []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return nil
	}

	due := time.Now().Add(24 * time.Hour)
	for _, d := range result.Data {
		title := fmt.Sprintf("Stale deal — needs attention: %s", d.Title)
		_ = agentS2SPost(ctx, a.hc, base+"/api/v1/tasks", a.serviceKey, map[string]any{
			"entity_type": "deal",
			"entity_id":   d.ID,
			"title":       title,
			"due_at":      due.Format(time.RFC3339),
			"priority":    "high",
			"tenant_id":   ev.TenantID,
		})
		body := "Deal flagged as stale by Vera AI (no activity in 14+ days)"
		_ = agentS2SPost(ctx, a.hc, base+"/api/v1/activities", a.serviceKey, map[string]any{
			"entity_type":   "deal",
			"entity_id":     d.ID,
			"activity_type": "note",
			"body":          body,
			"source":        "agent",
			"tenant_id":     ev.TenantID,
		})
	}

	if len(result.Data) > 0 {
		a.logger.Info("deal stale detector ran",
			zap.String("tenant", ev.TenantID),
			zap.Int("stale_deals", len(result.Data)),
		)
	}
	return nil
}

// ── NurtureSequenceEnhancer ──────────────────────────────────────────────────

// NurtureSequenceEnhancer triggers on crm.lead.qualified and enrolls
// the lead in the most appropriate nurture sequence.
type NurtureSequenceEnhancer struct {
	db         *ent.Client
	apiBaseURL string
	serviceKey string
	logger     *zap.Logger
	hc         *http.Client
}

func (a *NurtureSequenceEnhancer) Subject() string { return "crm.lead.qualified" }
func (a *NurtureSequenceEnhancer) Durable() string { return "marketflow-worker-agent-nurture-enhancer" }

func (a *NurtureSequenceEnhancer) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID string `json:"tenant_id"`
		LeadID   string `json:"lead_id"`
		Score    int    `json:"score"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return nil
	}
	if ev.LeadID == "" || a.db == nil {
		return nil
	}

	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return nil
	}
	leadID, err := uuid.Parse(ev.LeadID)
	if err != nil {
		return nil
	}
	_ = tenantID
	_ = leadID

	// Find best matching nurture sequence for this qualified lead via S2S
	base := strings.TrimSuffix(a.apiBaseURL, "/")
	seqURL := fmt.Sprintf("%s/api/v1/sequences?trigger_event=lead.qualified&active=true&limit=1&tenant_id=%s",
		base, ev.TenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, seqURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-API-Key", a.serviceKey)
	req.Header.Set("X-Tenant-ID", ev.TenantID)
	resp, err := a.hc.Do(req)
	if err != nil || resp.StatusCode >= 300 {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	var seqResult struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &seqResult); err != nil || len(seqResult.Data) == 0 {
		return nil
	}

	// Enroll lead in the sequence
	enrollURL := base + "/api/v1/sequences/" + seqResult.Data[0].ID + "/enroll"
	if err := agentS2SPost(ctx, a.hc, enrollURL, a.serviceKey, map[string]any{
		"lead_id":   ev.LeadID,
		"tenant_id": ev.TenantID,
		"source":    "vera-qualified-agent",
	}); err != nil {
		a.logger.Warn("failed to enroll qualified lead in sequence",
			zap.String("lead_id", ev.LeadID), zap.Error(err))
		return nil
	}

	a.logger.Info("qualified lead enrolled in nurture sequence",
		zap.String("lead_id", ev.LeadID),
		zap.String("sequence_id", seqResult.Data[0].ID),
	)
	return nil
}

// ── CustomAgentExecutionConsumer ─────────────────────────────────────────────

// CustomAgentExecutionConsumer subscribes to crm.agent.run_requested and executes
// custom agents by calling marketflow-ai /api/v1/agent/run (S2S).
type CustomAgentExecutionConsumer struct {
	db         *ent.Client
	aiBaseURL  string
	apiBaseURL string
	serviceKey string
	logger     *zap.Logger
	hc         *http.Client
}

func (c *CustomAgentExecutionConsumer) Subject() string { return "crm.agent.run_requested" }
func (c *CustomAgentExecutionConsumer) Durable() string { return "crm-agent-run-requested" }

func (c *CustomAgentExecutionConsumer) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		RunID    string         `json:"run_id"`
		AgentID  string         `json:"agent_id"`
		TenantID string         `json:"tenant_id"`
		Payload  map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(c.logger, msg, "custom agent consumer: unmarshal", err)
	}

	runID, err := uuid.Parse(ev.RunID)
	if err != nil {
		return dropPoison(c.logger, msg, "bad run_id", err)
	}
	agentID, err := uuid.Parse(ev.AgentID)
	if err != nil {
		return dropPoison(c.logger, msg, "bad agent_id", err)
	}
	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return dropPoison(c.logger, msg, "bad tenant_id", err)
	}

	run, err := c.db.AgentRun.Get(ctx, runID)
	if err != nil {
		if ent.IsNotFound(err) {
			return dropPoison(c.logger, msg, "agent run not found", err)
		}
		return fmt.Errorf("custom agent execution: get run: %w", err)
	}
	agent, err := c.db.CustomAgent.Get(ctx, agentID)
	if err != nil {
		if ent.IsNotFound(err) {
			c.finishRun(ctx, runID, "failed", "the agent no longer exists", nil)
			_ = msg.Ack()
			return nil
		}
		// Transient DB error: Nak via the wrapper so it retries.
		return fmt.Errorf("custom agent execution: get agent: %w", err)
	}
	if agent.TenantID != tenantID || run.TenantID != tenantID || run.CustomAgentID != agentID {
		return dropPoison(c.logger, msg, "custom agent run tenant mismatch", fmt.Errorf("run %s", runID))
	}
	// Manual runs work on inactive agents (testing before switching one on);
	// schedule and event triggers only run active agents.
	if !agent.IsActive && run.TriggeredBy != "manual" {
		c.finishRun(ctx, runID, "failed", "the agent is switched off", nil)
		_ = msg.Ack()
		return nil
	}

	// Claim the run: pending to running, once. A redelivered message finds it
	// claimed and stops, so an agent never runs twice for one trigger.
	claimed, err := c.db.AgentRun.Update().
		Where(agentrun.ID(runID), agentrun.StatusEQ("pending")).
		SetStatus("running").
		Save(ctx)
	if err != nil {
		return fmt.Errorf("custom agent execution: claim run: %w", err)
	}
	if claimed == 0 {
		_ = msg.Ack()
		return nil
	}

	payload := map[string]any{
		"run_id":         runID.String(),
		"system_prompt":  agent.SystemPrompt,
		"allowed_tools":  agent.AllowedTools,
		"max_iterations": agent.MaxIterations,
		"tenant_id":      ev.TenantID,
		"context":        ev.Payload,
		// Automatic (event and schedule) runs cost the tenant one AI credit,
		// charged by marketflow-ai; Run now stays free.
		"billable": run.TriggeredBy != "manual",
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var out agentRunResponse
	if err := agentS2SCall(callCtx, c.hc, c.aiBaseURL+"/api/v1/agent/run", c.serviceKey, payload, &out); err != nil {
		c.logger.Warn("custom agent AI call failed", zap.String("run_id", runID.String()), zap.Error(err))
		c.finishRun(ctx, runID, "failed", err.Error(), nil)
		_ = msg.Ack()
		return nil
	}
	status := "completed"
	errMsg := ""
	if out.Status != "completed" {
		status, errMsg = "failed", out.Error
	}
	c.finishRun(ctx, runID, status, errMsg, &out)
	if status == "completed" {
		_, _ = c.db.CustomAgent.UpdateOneID(agentID).AddRunCount(1).SetLastRunAt(time.Now()).Save(ctx)
	}
	c.logger.Info("custom agent run finished", zap.String("run_id", runID.String()), zap.String("status", status),
		zap.Int("tokens", out.TotalTokens), zap.Int("tool_calls", out.ToolCalls))
	_ = msg.Ack()
	return nil
}

// agentRunResponse is marketflow-ai's /api/v1/agent/run reply.
type agentRunResponse struct {
	Status       string `json:"status"`
	Error        string `json:"error"`
	Answer       string `json:"answer"`
	Trace        any    `json:"trace"`
	TotalTokens  int    `json:"total_tokens"`
	ToolCalls    int    `json:"tool_calls"`
	Iterations   int    `json:"iterations"`
	StoppedEarly bool   `json:"stopped_early"`
	StopReason   string `json:"stop_reason"`
}

// finishRun records the outcome; the answer and the loop trace go into the
// run's trace JSON so the run history shows what the agent did.
func (c *CustomAgentExecutionConsumer) finishRun(ctx context.Context, runID uuid.UUID, status, errMsg string, out *agentRunResponse) {
	upd := c.db.AgentRun.UpdateOneID(runID).SetStatus(status).SetFinishedAt(time.Now())
	if errMsg != "" {
		upd.SetErrorMessage(truncateStr(errMsg, 1000))
	}
	if out != nil {
		upd.SetTotalTokens(out.TotalTokens).SetToolCallsN(out.ToolCalls).SetTrace(map[string]any{
			"answer":        truncateStr(out.Answer, 8000),
			"iterations":    out.Iterations,
			"steps":         out.Trace,
			"stopped_early": out.StoppedEarly,
			"stop_reason":   out.StopReason,
		})
	}
	if _, err := upd.Save(ctx); err != nil {
		c.logger.Error("custom agent run: record outcome", zap.String("run_id", runID.String()), zap.Error(err))
	}
}

// agentS2SCall posts body and decodes the JSON reply into out.
func agentS2SCall(ctx context.Context, hc *http.Client, url, serviceKey string, body map[string]any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", serviceKey)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("s2s %s: status %d: %s", url, resp.StatusCode, truncateStr(string(rb), 200))
	}
	if err := json.Unmarshal(rb, out); err != nil {
		return fmt.Errorf("s2s %s: decode: %w", url, err)
	}
	return nil
}

// ── CronAgentRunner ──────────────────────────────────────────────────────────

// CronAgentRunnerConfig configures the cron-based background agent runner.
type CronAgentRunnerConfig struct {
	DB             *ent.Client
	APIBaseURL     string
	ServiceKey     string
	FollowUpDays   int
	StaleDealDays  int
	FollowUpEvery  time.Duration // default 1 hour
	StaleDealEvery time.Duration // default 24 hours
	Logger         *zap.Logger
}

// CronAgentRunner runs ProactiveFollowUpAgent and DealStaleDetector on cron ticks.
// It enumerates all active tenant IDs from the DB and runs each agent per tenant.
type CronAgentRunner struct {
	cfg CronAgentRunnerConfig
}

// NewCronAgentRunner constructs a CronAgentRunner with sensible defaults.
func NewCronAgentRunner(cfg CronAgentRunnerConfig) *CronAgentRunner {
	if cfg.FollowUpDays <= 0 {
		cfg.FollowUpDays = 7
	}
	if cfg.StaleDealDays <= 0 {
		cfg.StaleDealDays = 14
	}
	if cfg.FollowUpEvery <= 0 {
		cfg.FollowUpEvery = 1 * time.Hour
	}
	if cfg.StaleDealEvery <= 0 {
		cfg.StaleDealEvery = 24 * time.Hour
	}
	return &CronAgentRunner{cfg: cfg}
}

// Run blocks until ctx is cancelled, firing agents on their configured schedules.
func (r *CronAgentRunner) Run(ctx context.Context) {
	hc := &http.Client{Timeout: 15 * time.Second}
	followupAgent := &ProactiveFollowUpAgent{
		db:         r.cfg.DB,
		apiBaseURL: r.cfg.APIBaseURL,
		serviceKey: r.cfg.ServiceKey,
		logger:     r.cfg.Logger.Named("cron.followup"),
		hc:         hc,
	}
	staleAgent := &DealStaleDetector{
		apiBaseURL: r.cfg.APIBaseURL,
		serviceKey: r.cfg.ServiceKey,
		logger:     r.cfg.Logger.Named("cron.deal-stale"),
		hc:         hc,
	}

	followupTick := time.NewTicker(r.cfg.FollowUpEvery)
	staleTick := time.NewTicker(r.cfg.StaleDealEvery)
	defer followupTick.Stop()
	defer staleTick.Stop()

	r.cfg.Logger.Info("cron agent runner started",
		zap.Duration("followup_every", r.cfg.FollowUpEvery),
		zap.Duration("stale_every", r.cfg.StaleDealEvery),
	)

	for {
		select {
		case <-ctx.Done():
			r.cfg.Logger.Info("cron agent runner stopped")
			return
		case <-followupTick.C:
			r.runPerTenant(ctx, followupAgent.Handle, r.cfg.FollowUpDays, "followup", r.cfg.FollowUpEvery)
		case <-staleTick.C:
			r.runPerTenant(ctx, staleAgent.Handle, r.cfg.StaleDealDays, "stale-deal", r.cfg.StaleDealEvery)
		}
	}
}

// runPerTenant enumerates distinct tenant_ids from the leads table and fires the agent per tenant.
func (r *CronAgentRunner) runPerTenant(ctx context.Context, handle func(context.Context, jetstream.Msg) error, days int, name string, every time.Duration) {
	if r.cfg.DB == nil {
		return
	}
	// The worker runs in every pod; one pod per period fires each agent.
	if !sharedcache.ClaimPeriod(ctx, "marketflow:cron-agent:"+name, every) {
		return
	}
	// Tenants using the CRM are those with at least one lead. Walk the tenants
	// table in keyset pages and check each with an indexed LIMIT 1 lookup
	// (leads(tenant_id, ...)), so the cost follows the tenant count, never the
	// lead count. (It used to load every lead of every tenant each run.)
	err := forEachCRMTenant(ctx, r.cfg.DB, func(tenantID uuid.UUID) {
		payload, _ := json.Marshal(map[string]any{
			"tenant_id":      tenantID.String(),
			"follow_up_days": days,
			"stale_days":     days,
		})
		if err := handle(ctx, &syntheticMsg{data: payload}); err != nil {
			r.cfg.Logger.Warn("cron agent error",
				zap.String("agent", name),
				zap.String("tenant", tenantID.String()),
				zap.Error(err),
			)
		}
	})
	if err != nil {
		r.cfg.Logger.Warn("cron agent tenant scan failed", zap.String("agent", name), zap.Error(err))
	}
}

// crmTenantPage bounds one keyset page of the tenant walk.
const crmTenantPage = 200

// forEachCRMTenant calls fn for every tenant with at least one lead, in id
// order, a page of tenants at a time.
func forEachCRMTenant(ctx context.Context, db *ent.Client, fn func(uuid.UUID)) error {
	var after uuid.UUID
	for {
		q := db.Tenant.Query().Order(ent.Asc(tenant.FieldID)).Limit(crmTenantPage)
		if after != uuid.Nil {
			q = q.Where(tenant.IDGT(after))
		}
		ids, err := q.IDs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			has, err := db.Lead.Query().Where(lead.TenantID(id)).Exist(ctx)
			if err != nil {
				return err
			}
			if has {
				fn(id)
			}
		}
		if len(ids) < crmTenantPage {
			return nil
		}
		after = ids[len(ids)-1]
	}
}

// syntheticMsg is a minimal jetstream.Msg implementation for cron-triggered agents.
type syntheticMsg struct{ data []byte }

func (m *syntheticMsg) Data() []byte                              { return m.data }
func (m *syntheticMsg) Ack() error                                { return nil }
func (m *syntheticMsg) Nak() error                                { return nil }
func (m *syntheticMsg) NakWithDelay(d time.Duration) error        { return nil }
func (m *syntheticMsg) InProgress() error                         { return nil }
func (m *syntheticMsg) Term() error                               { return nil }
func (m *syntheticMsg) TermWithReason(reason string) error        { return nil }
func (m *syntheticMsg) Subject() string                           { return "" }
func (m *syntheticMsg) Headers() nats.Header                      { return nil }
func (m *syntheticMsg) Reply() string                             { return "" }
func (m *syntheticMsg) DoubleAck(ctx context.Context) error       { return nil }
func (m *syntheticMsg) Metadata() (*jetstream.MsgMetadata, error) { return nil, nil }
