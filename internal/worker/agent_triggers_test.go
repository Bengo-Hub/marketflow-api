package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
)

func seedAgent(t *testing.T, db *ent.Client, tid uuid.UUID, active bool, trigger string, cfg map[string]any) *ent.CustomAgent {
	t.Helper()
	a, err := db.CustomAgent.Create().SetTenantID(tid).SetName("A " + trigger).SetTriggerType(trigger).
		SetTriggerConfig(cfg).SetIsActive(active).Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func runsOf(t *testing.T, db *ent.Client, agentID uuid.UUID) []*ent.AgentRun {
	t.Helper()
	rows, err := db.AgentRun.Query().Where(agentrun.CustomAgentID(agentID)).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestEventTriggerStartsMatchingAgentsOnceAcrossRedelivery(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "agt-evt")
	onLead := seedAgent(t, db, tid, true, custom_agents.TriggerEvent, map[string]any{"event": "lead.created"})
	onQualified := seedAgent(t, db, tid, true, custom_agents.TriggerEvent, map[string]any{"event": "crm.lead.qualified"})
	switchedOff := seedAgent(t, db, tid, false, custom_agents.TriggerEvent, map[string]any{"event": "lead.created"})
	manual := seedAgent(t, db, tid, true, custom_agents.TriggerManual, map[string]any{})

	svc := custom_agents.NewService(db, zap.NewNop())
	var h *AgentEventTrigger
	for _, x := range NewAgentEventTriggers(svc, zap.NewNop()) {
		if x.Subject() == leads.LeadCreatedTopic {
			h = x.(*AgentEventTrigger)
		}
	}
	lid := uuid.New()
	msg := fakeMsg{data: envelope(t, tid, lid, map[string]any{"lead_id": lid.String(), "source": "website"})}
	for i := 0; i < 2; i++ { // the second delivery must not start it again
		if err := h.Handle(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	runs := runsOf(t, db, onLead.ID)
	if len(runs) != 1 || runs[0].TriggeredBy != "event:lead.created" || runs[0].TriggerPayload["lead_id"] != lid.String() {
		t.Fatalf("lead agent runs = %+v", runs)
	}
	for _, other := range []*ent.CustomAgent{onQualified, switchedOff, manual} {
		if n := len(runsOf(t, db, other.ID)); n != 0 {
			t.Fatalf("agent %s (%s) started %d times", other.Name, other.TriggerType, n)
		}
	}

	// A new lead is a new event.
	lid2 := uuid.New()
	_ = h.Handle(ctx, fakeMsg{data: envelope(t, tid, lid2, map[string]any{"lead_id": lid2.String()})})
	if n := len(runsOf(t, db, onLead.ID)); n != 2 {
		t.Fatalf("second lead: %d runs", n)
	}
}

func TestQualifiedEventFlatPayload(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "agt-qual")
	a := seedAgent(t, db, tid, true, custom_agents.TriggerEvent, map[string]any{"event": "crm.lead.qualified"})
	svc := custom_agents.NewService(db, zap.NewNop())
	h := NewAgentEventTriggers(svc, zap.NewNop())[1]
	lid := uuid.New()
	// The scorer publishes a flat body, not an outbox envelope.
	raw, _ := json.Marshal(map[string]any{"tenant_id": tid.String(), "lead_id": lid.String(), "score": 82})
	if err := h.Handle(ctx, fakeMsg{data: raw}); err != nil {
		t.Fatal(err)
	}
	runs := runsOf(t, db, a.ID)
	if len(runs) != 1 || runs[0].TriggerPayload["score"] != float64(82) {
		t.Fatalf("runs = %+v", runs)
	}
}

// A deal can move many times: each move (move_id) starts the agent once, and a
// redelivered move does not.
func TestDealStageTriggerDedupesOnMove(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "agt-deal")
	a := seedAgent(t, db, tid, true, custom_agents.TriggerEvent, map[string]any{"event": "crm.deal.stage_changed"})
	svc := custom_agents.NewService(db, zap.NewNop())
	var h Handler
	for _, x := range NewAgentEventTriggers(svc, zap.NewNop()) {
		if x.Subject() == "crm.deal.stage_changed" {
			h = x
		}
	}
	dealID := uuid.New()
	move := func(moveID string) fakeMsg {
		raw, _ := json.Marshal(map[string]any{
			"event_type": "deal.stage_changed", "tenant_id": tid.String(),
			"payload": map[string]any{"deal_id": dealID.String(), "move_id": moveID, "to_stage_id": uuid.NewString()},
		})
		return fakeMsg{data: raw}
	}
	m1, m2 := uuid.NewString(), uuid.NewString()
	for _, m := range []fakeMsg{move(m1), move(m1), move(m2)} {
		if err := h.Handle(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(runsOf(t, db, a.ID)); n != 2 {
		t.Fatalf("want one run per move (2), got %d", n)
	}
}

func TestDailyCapStopsAutomaticRunsOnly(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "agt-cap")
	a := seedAgent(t, db, tid, true, custom_agents.TriggerEvent, map[string]any{"event": "lead.created"})
	for i := 0; i < custom_agents.AutoDailyCap; i++ {
		if _, err := db.AgentRun.Create().SetTenantID(tid).SetCustomAgentID(a.ID).SetTriggeredBy("event:lead.created").
			SetTriggerPayload(map[string]any{"lead_id": uuid.NewString()}).SetStatus("completed").SetTrace(map[string]any{}).
			SetStartedAt(time.Now().Add(-time.Hour)).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	svc := custom_agents.NewService(db, zap.NewNop())
	if _, err := svc.TriggerAuto(ctx, tid, a.ID, "event:lead.created", map[string]any{"lead_id": uuid.NewString()}); err != custom_agents.ErrDailyCap {
		t.Fatalf("want the daily cap, got %v", err)
	}
	// Run now is never capped.
	if _, err := svc.TriggerRun(ctx, tid, a.ID, custom_agents.TriggerManual, nil); err != nil {
		t.Fatalf("manual run blocked: %v", err)
	}
}

func TestScheduleStartsOncePerPeriodAtLocalTime(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "agt-sched")
	if _, err := db.Tenant.UpdateOneID(tid).SetTimezone("Africa/Nairobi").Save(ctx); err != nil {
		t.Fatal(err)
	}
	// Runs are stamped with the real clock (started_at is immutable), so the
	// fake clock is anchored to today in Nairobi.
	nbo, _ := time.LoadLocation("Africa/Nairobi")
	wall := time.Now().In(nbo)
	today := time.Date(wall.Year(), wall.Month(), wall.Day(), 0, 0, 0, 0, nbo)
	daily := seedAgent(t, db, tid, true, custom_agents.TriggerSchedule, map[string]any{"frequency": "daily", "time": "00:01"})
	weekly := seedAgent(t, db, tid, true, custom_agents.TriggerSchedule,
		map[string]any{"frequency": "weekly", "time": "00:01", "weekday": float64(today.Weekday())})

	svc := custom_agents.NewService(db, zap.NewNop())
	j := NewAgentSchedules(db, svc, zap.NewNop())
	j.claim = func(context.Context, string, time.Duration) bool { return true }
	// 00:00 today: nothing is due yet.
	j.nowFn = func() time.Time { return today }
	if _, err := j.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runsOf(t, db, daily.ID))+len(runsOf(t, db, weekly.ID)) != 0 {
		t.Fatal("ran before its time")
	}
	// Now (past 00:01): both are due; the next tick starts nothing new.
	j.nowFn = func() time.Time { return wall }
	for i := 0; i < 2; i++ {
		if _, err := j.tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(runsOf(t, db, daily.ID)) != 1 || len(runsOf(t, db, weekly.ID)) != 1 {
		t.Fatalf("daily=%d weekly=%d, want one each", len(runsOf(t, db, daily.ID)), len(runsOf(t, db, weekly.ID)))
	}
	// Tomorrow 00:05: the daily agent runs again, the weekly one does not.
	j.nowFn = func() time.Time { return today.AddDate(0, 0, 1).Add(5 * time.Minute) }
	if _, err := j.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runsOf(t, db, daily.ID)) != 2 || len(runsOf(t, db, weekly.ID)) != 1 {
		t.Fatalf("tuesday: daily=%d weekly=%d", len(runsOf(t, db, daily.ID)), len(runsOf(t, db, weekly.ID)))
	}
}
