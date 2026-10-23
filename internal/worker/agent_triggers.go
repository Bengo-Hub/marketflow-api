package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
)

// Custom agents run on three triggers: manual (Run now), an event, or a
// schedule. These two pieces start the automatic ones through
// custom_agents.TriggerAuto, which checks the agent is on and under its daily
// cap; marketflow-ai charges each automatic run one credit.

// claimFn claims a once-only key; sharedcache.ClaimOnce in production.
type claimFn func(ctx context.Context, key string, ttl time.Duration) bool

// AgentEventTrigger starts the tenant's event agents when subject arrives.
type AgentEventTrigger struct {
	subject string
	durable string
	agents  *custom_agents.Service
	logger  *zap.Logger
}

// NewAgentEventTriggers returns one handler per supported event.
func NewAgentEventTriggers(agents *custom_agents.Service, logger *zap.Logger) []Handler {
	return []Handler{
		&AgentEventTrigger{subject: leads.LeadCreatedTopic, durable: "marketflow-worker-agent-trigger-lead-created",
			agents: agents, logger: logger.Named("agent.trigger.lead-created")},
		&AgentEventTrigger{subject: "crm.lead.qualified", durable: "marketflow-worker-agent-trigger-lead-qualified",
			agents: agents, logger: logger.Named("agent.trigger.lead-qualified")},
	}
}

func (h *AgentEventTrigger) Subject() string { return h.subject }
func (h *AgentEventTrigger) Durable() string { return h.durable }

// Handle starts every matching agent once for this event. The runs table is
// the record of what started: an agent that already has a run for this event
// and lead is skipped, so a redelivered message never runs it twice, and one
// whose start failed (a DB blip) has no run yet and is retried with the message.
// Nothing depends on Redis, so a Redis outage never drops a trigger.
func (h *AgentEventTrigger) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID string `json:"tenant_id"`
		LeadID   string `json:"lead_id"`
		Score    int    `json:"score"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "agent trigger: decode", err)
	}
	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return dropPoison(h.logger, msg, "agent trigger: bad tenant_id", err)
	}
	if _, err := uuid.Parse(ev.LeadID); err != nil {
		return dropPoison(h.logger, msg, "agent trigger: bad lead_id", err)
	}
	agents, err := h.agents.AgentsForEvent(ctx, tenantID, h.subject)
	if err != nil {
		return fmt.Errorf("agent trigger: list agents: %w", err)
	}
	payload := map[string]any{"event": h.subject, "lead_id": ev.LeadID}
	if ev.Score > 0 {
		payload["score"] = ev.Score
	}
	var retry error
	for _, a := range agents {
		ran, err := h.agents.RanForEvent(ctx, tenantID, a.ID, h.subject, ev.LeadID)
		if err != nil {
			retry = err
			continue
		}
		if ran {
			continue // already started for this event
		}
		if _, err := h.agents.TriggerAuto(ctx, tenantID, a.ID, "event:"+h.subject, payload); err != nil {
			switch {
			case errors.Is(err, custom_agents.ErrDailyCap), errors.Is(err, custom_agents.ErrNotActive):
				h.logger.Info("agent not started", zap.String("agent_id", a.ID.String()), zap.Error(err))
			default:
				retry = err
				h.logger.Warn("agent start failed; the event will be retried",
					zap.String("agent_id", a.ID.String()), zap.Error(err))
			}
		}
	}
	return retry
}

// AgentSchedules starts schedule agents at their tenant-local time, once per
// day (daily) or week (weekly), fleet-wide through a claim per period. A run
// missed while the worker was down starts on the next tick the same day.
type AgentSchedules struct {
	db     *ent.Client
	agents *custom_agents.Service
	every  time.Duration
	claim  claimFn
	nowFn  func() time.Time
	logger *zap.Logger
}

// NewAgentSchedules builds the job; it ticks every five minutes.
func NewAgentSchedules(db *ent.Client, agents *custom_agents.Service, logger *zap.Logger) *AgentSchedules {
	return &AgentSchedules{db: db, agents: agents, every: 5 * time.Minute, claim: sharedcache.ClaimOnce,
		nowFn: time.Now, logger: logger.Named("agent.schedules")}
}

// Run ticks until ctx ends.
func (j *AgentSchedules) Run(ctx context.Context) {
	t := time.NewTicker(j.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if started, err := j.tick(ctx); err != nil {
				j.logger.Warn("agent schedule scan failed", zap.Error(err))
			} else if started > 0 {
				j.logger.Info("scheduled agents started", zap.Int("runs", started))
			}
		}
	}
}

const schedulePage = 500

// tick walks switched-on schedule agents in id order (keyset pages) and starts
// the ones that are due.
func (j *AgentSchedules) tick(ctx context.Context) (int, error) {
	started := 0
	zones := map[uuid.UUID]*time.Location{}
	var after uuid.UUID
	for {
		q := j.db.CustomAgent.Query().
			Where(customagent.IsActive(true), customagent.TriggerType(custom_agents.TriggerSchedule)).
			Order(ent.Asc(customagent.FieldID)).
			Limit(schedulePage)
		if after != uuid.Nil {
			q = q.Where(customagent.IDGT(after))
		}
		page, err := q.All(ctx)
		if err != nil {
			return started, err
		}
		for _, a := range page {
			if j.startIfDue(ctx, a, zones) {
				started++
			}
		}
		if len(page) < schedulePage {
			return started, nil
		}
		after = page[len(page)-1].ID
	}
}

func (j *AgentSchedules) startIfDue(ctx context.Context, a *ent.CustomAgent, zones map[uuid.UUID]*time.Location) bool {
	sch, err := custom_agents.ParseSchedule(a.TriggerConfig)
	if err != nil {
		return false // saved before validation existed; the settings page shows it as manual
	}
	loc, ok := zones[a.TenantID]
	if !ok {
		loc = j.tenantZone(ctx, a.TenantID)
		zones[a.TenantID] = loc
	}
	now := j.nowFn().In(loc)
	if sch.Frequency == "weekly" && int(now.Weekday()) != sch.Weekday {
		return false
	}
	if now.Before(sch.At(now)) {
		return false
	}
	// The run time today is the period start: a daily agent runs once a day, a
	// weekly one once on its weekday.
	periodStart := sch.At(now)
	ran, err := j.agents.RanScheduleSince(ctx, a.TenantID, a.ID, periodStart)
	if err != nil || ran {
		return false
	}
	// Short guard so two worker replicas in the same tick do not both start it;
	// it expires before the next tick, so a failed start is retried then.
	if !j.claim(ctx, "marketflow:agent-schedule:"+a.ID.String()+":"+periodStart.Format(time.RFC3339), j.every-30*time.Second) {
		return false
	}
	_, err = j.agents.TriggerAuto(ctx, a.TenantID, a.ID, custom_agents.TriggerSchedule,
		map[string]any{"schedule": sch.Frequency, "local_time": now.Format(time.RFC3339)})
	if err != nil {
		j.logger.Warn("scheduled agent not started", zap.String("agent_id", a.ID.String()), zap.Error(err))
		return false
	}
	return true
}

// tenantZone is the tenant's time zone, Africa/Nairobi when unset or invalid.
func (j *AgentSchedules) tenantZone(ctx context.Context, tenantID uuid.UUID) *time.Location {
	if t, err := j.db.Tenant.Get(ctx, tenantID); err == nil && t.Timezone != nil && *t.Timezone != "" {
		if loc, err := time.LoadLocation(*t.Timezone); err == nil {
			return loc
		}
	}
	if loc, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return loc
	}
	return time.FixedZone("EAT", 3*3600)
}
