package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

func openWorkerTestDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWorkerPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedTenant(t *testing.T, db *ent.Client, prefix string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName(prefix + " " + tid.String()).SetSlug(prefix + "-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.AgentRun.Delete().Where(agentrun.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	return tid
}

func TestForEachCRMTenantVisitsOnlyTenantsWithLeads(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	withLeads := seedTenant(t, db, "crm-yes")
	withoutLeads := seedTenant(t, db, "crm-no")
	if _, err := db.Lead.Create().SetTenantID(withLeads).SetSource("website").SetLeadType("customer").SetStatus("new").SetFirstName("A").Save(ctx); err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]int{}
	if err := forEachCRMTenant(ctx, db, func(id uuid.UUID) { seen[id]++ }); err != nil {
		t.Fatal(err)
	}
	if seen[withLeads] != 1 {
		t.Fatalf("tenant with leads visited %d times", seen[withLeads])
	}
	if seen[withoutLeads] != 0 {
		t.Fatal("tenant without leads was visited")
	}
}

func TestCustomAgentRunRecordsTraceOnceAndRespectsSwitchOff(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "agentrun")

	var calls atomic.Int32
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/agent/run" || r.Header.Get("X-API-Key") != "svc" {
			t.Errorf("unexpected call %s key=%q", r.URL.Path, r.Header.Get("X-API-Key"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if tools, _ := body["allowed_tools"].([]any); len(tools) != 1 || tools[0] != "kb_search" {
			t.Errorf("allowed_tools = %v", body["allowed_tools"])
		}
		_, _ = w.Write([]byte(`{"status":"completed","answer":"3 follow-ups drafted","trace":[{"iteration":1}],"total_tokens":420,"tool_calls":2,"iterations":2}`))
	}))
	defer ai.Close()

	agent, err := db.CustomAgent.Create().SetTenantID(tid).SetName("Follow-up drafter").SetIsActive(false).
		SetAllowedTools([]string{"kb_search"}).SetMaxIterations(4).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newRun := func(trigger string) uuid.UUID {
		run, err := db.AgentRun.Create().SetTenantID(tid).SetCustomAgentID(agent.ID).SetTriggeredBy(trigger).
			SetStatus("pending").SetTrace(map[string]any{}).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	c := &CustomAgentExecutionConsumer{db: db, aiBaseURL: ai.URL, serviceKey: "svc", logger: zap.NewNop(), hc: ai.Client()}
	msgFor := func(runID uuid.UUID) *syntheticMsg {
		b, _ := json.Marshal(map[string]any{"run_id": runID.String(), "agent_id": agent.ID.String(), "tenant_id": tid.String(), "payload": map[string]any{"why": "test"}})
		return &syntheticMsg{data: b}
	}

	// A manual run works even though the agent is switched off.
	manual := newRun("manual")
	if err := c.Handle(ctx, msgFor(manual)); err != nil {
		t.Fatal(err)
	}
	got, _ := db.AgentRun.Get(ctx, manual)
	if got.Status != "completed" || got.TotalTokens != 420 || got.ToolCallsN != 2 || got.Trace["answer"] != "3 follow-ups drafted" || got.FinishedAt == nil {
		t.Fatalf("run = %+v", got)
	}
	if a, _ := db.CustomAgent.Get(ctx, agent.ID); a.RunCount != 1 || a.LastRunAt == nil {
		t.Fatalf("agent counters = %d %v", a.RunCount, a.LastRunAt)
	}
	// Redelivery of the same message must not call the AI again.
	if err := c.Handle(ctx, msgFor(manual)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("AI called %d times, want 1", calls.Load())
	}

	// A scheduled run of a switched-off agent fails without calling the AI.
	scheduled := newRun("cron")
	if err := c.Handle(ctx, msgFor(scheduled)); err != nil {
		t.Fatal(err)
	}
	got, _ = db.AgentRun.Get(ctx, scheduled)
	if got.Status != "failed" || got.ErrorMessage == nil || calls.Load() != 1 {
		t.Fatalf("switched-off run = %+v calls=%d", got, calls.Load())
	}
}
