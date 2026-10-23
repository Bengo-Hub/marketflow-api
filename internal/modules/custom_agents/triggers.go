package custom_agents

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
)

// Trigger types. Rows saved by the old UI with other values ("on_lead_created",
// "scheduled") are treated as manual: nothing dispatches them.
const (
	TriggerManual   = "manual"
	TriggerEvent    = "event"
	TriggerSchedule = "schedule"
)

// Event is an event an agent can run on.
type Event struct {
	Label string
	// Key is the payload field that identifies one occurrence: a redelivered
	// event with the same value never starts the agent twice.
	Key string
}

// Events lists the events marketflow-api emits that agents can run on.
var Events = map[string]Event{
	"lead.created":           {Label: "A new lead arrives", Key: "lead_id"},
	"crm.lead.qualified":     {Label: "A lead becomes qualified (score reaches 70)", Key: "lead_id"},
	"crm.contact.created":    {Label: "A new contact is added", Key: "contact_id"},
	"crm.deal.stage_changed": {Label: "A deal moves to another stage", Key: "move_id"},
}

// AutoDailyCap is the most automatic (event or schedule) runs one agent may
// start in 24 hours. Each automatic run costs one AI credit.
const AutoDailyCap = 50

// ErrDailyCap means the agent has used today's automatic runs.
var ErrDailyCap = errors.New("custom agent: daily automatic run limit reached")

// ErrNotActive means the agent is switched off, so triggers skip it.
var ErrNotActive = errors.New("custom agent: switched off")

var clockRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// Schedule is the trigger_config of a schedule trigger.
type Schedule struct {
	Frequency string // daily | weekly
	Time      string // HH:MM, tenant local time
	Weekday   int    // 0 Sunday to 6 Saturday, weekly only
}

// ParseSchedule reads and validates a schedule trigger_config.
func ParseSchedule(cfg map[string]any) (Schedule, error) {
	s := Schedule{Frequency: str(cfg["frequency"]), Time: str(cfg["time"])}
	if s.Frequency != "daily" && s.Frequency != "weekly" {
		return s, fmt.Errorf("schedule frequency must be daily or weekly")
	}
	if !clockRe.MatchString(s.Time) {
		return s, fmt.Errorf("schedule time must be HH:MM (24-hour)")
	}
	if s.Frequency == "weekly" {
		d, ok := cfg["weekday"].(float64) // JSON numbers decode as float64
		if n, isInt := cfg["weekday"].(int); isInt {
			d, ok = float64(n), true
		}
		if !ok || d < 0 || d > 6 || d != float64(int(d)) {
			return s, fmt.Errorf("weekly schedules need a weekday from 0 (Sunday) to 6 (Saturday)")
		}
		s.Weekday = int(d)
	}
	return s, nil
}

// At returns the run time on the given local day.
func (s Schedule) At(day time.Time) time.Time {
	var h, m int
	_, _ = fmt.Sscanf(s.Time, "%d:%d", &h, &m)
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location())
}

// ValidateTrigger checks a trigger type and its config.
func ValidateTrigger(triggerType string, cfg map[string]any) error {
	switch triggerType {
	case TriggerManual:
		return nil
	case TriggerEvent:
		if _, ok := Events[str(cfg["event"])]; !ok {
			return fmt.Errorf("event triggers need trigger_config.event, one of: lead.created, crm.lead.qualified, crm.contact.created, crm.deal.stage_changed")
		}
		return nil
	case TriggerSchedule:
		_, err := ParseSchedule(cfg)
		return err
	default:
		return fmt.Errorf("trigger_type must be manual, event or schedule")
	}
}

// AgentsForEvent lists the tenant's switched-on agents that run on event.
func (s *Service) AgentsForEvent(ctx context.Context, tenantID uuid.UUID, event string) ([]*ent.CustomAgent, error) {
	return s.client.CustomAgent.Query().
		Where(
			customagent.TenantID(tenantID),
			customagent.IsActive(true),
			customagent.TriggerType(TriggerEvent),
			func(sel *sql.Selector) { sel.Where(sqljson.ValueEQ(customagent.FieldTriggerConfig, event, sqljson.Path("event"))) },
			notTeamMember(),
		).
		Limit(50). // bounds bad data; a tenant has a handful of agents
		All(ctx)
}

// TriggerAuto starts an automatic (event or schedule) run, enforcing that the
// agent is switched on and under its daily cap. triggeredBy is "event:<name>"
// or "schedule"; the run is billed by marketflow-ai because it is not manual.
func (s *Service) TriggerAuto(ctx context.Context, tenantID, agentID uuid.UUID, triggeredBy string, payload map[string]any) (*ent.AgentRun, error) {
	a, err := s.Get(ctx, tenantID, agentID)
	if err != nil {
		return nil, err
	}
	if !a.IsActive {
		return nil, ErrNotActive
	}
	n, err := s.client.AgentRun.Query().
		Where(
			agentrun.TenantID(tenantID),
			agentrun.CustomAgentID(agentID),
			agentrun.TriggeredByNEQ(TriggerManual),
			agentrun.StartedAtGTE(time.Now().Add(-24*time.Hour)),
		).
		Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("count automatic runs: %w", err)
	}
	if n >= AutoDailyCap {
		return nil, ErrDailyCap
	}
	return s.TriggerRun(ctx, tenantID, agentID, triggeredBy, payload)
}

// RanForEvent reports whether the agent already has a run for this occurrence
// of event (matched on the event's Key field, for example lead_id), so a
// redelivered event never starts it twice. The runs table is the record: a
// start that failed left no row and is retried.
func (s *Service) RanForEvent(ctx context.Context, tenantID, agentID uuid.UUID, event, occurrence string) (bool, error) {
	ev, ok := Events[event]
	if !ok {
		return false, fmt.Errorf("custom agent: unknown event %q", event)
	}
	return s.client.AgentRun.Query().
		Where(
			agentrun.TenantID(tenantID),
			agentrun.CustomAgentID(agentID),
			agentrun.TriggeredBy("event:"+event),
			func(sel *sql.Selector) {
				sel.Where(sqljson.ValueEQ(agentrun.FieldTriggerPayload, occurrence, sqljson.Path(ev.Key)))
			},
		).
		Exist(ctx)
}

// RanScheduleSince reports whether the agent has a scheduled run started at or
// after since (the start of the current day or week).
func (s *Service) RanScheduleSince(ctx context.Context, tenantID, agentID uuid.UUID, since time.Time) (bool, error) {
	return s.client.AgentRun.Query().
		Where(
			agentrun.TenantID(tenantID),
			agentrun.CustomAgentID(agentID),
			agentrun.TriggeredBy(TriggerSchedule),
			agentrun.StartedAtGTE(since),
		).
		Exist(ctx)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
