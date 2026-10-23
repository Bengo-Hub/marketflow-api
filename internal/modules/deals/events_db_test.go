package deals

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
	"github.com/bengobox/marketflow-api/internal/ent/pipeline"
	"github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
)

const testDealsPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openDealsDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testDealsPostgresDSN)
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

func stageEvents(t *testing.T, db *ent.Client, dealID uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := db.OutboxEvent.Query().Where(outboxevent.AggregateID(dealID), outboxevent.EventType("deal.stage_changed")).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		var env struct {
			AggregateType string         `json:"aggregate_type"`
			Payload       map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(r.Payload, &env); err != nil {
			t.Fatal(err)
		}
		if r.AggregateType != "crm" {
			t.Fatalf("aggregate = %q; the subject must stay under crm.>", r.AggregateType)
		}
		out = append(out, env.Payload)
	}
	return out
}

func TestStageMovesEmitOneEventEach(t *testing.T) {
	db := openDealsDB(t)
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("D " + tid.String()).SetSlug("d-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := db.Pipeline.Create().SetTenantID(tid).SetName("Sales").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := db.PipelineStage.Create().SetTenantID(tid).SetPipelineID(p.ID).SetName("New").SetStageOrder(1).Save(ctx)
	s2, _ := db.PipelineStage.Create().SetTenantID(tid).SetPipelineID(p.ID).SetName("Proposal").SetStageOrder(2).Save(ctx)
	t.Cleanup(func() {
		_, _ = db.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = db.Deal.Delete().Where(deal.TenantID(tid)).Exec(ctx)
		_, _ = db.PipelineStage.Delete().Where(pipelinestage.TenantID(tid)).Exec(ctx)
		_, _ = db.Pipeline.Delete().Where(pipeline.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})

	svc := NewService(db, zap.NewNop())
	d, err := svc.Create(ctx, CreateInput{TenantID: tid, Title: "Order", PipelineID: p.ID, StageID: s1.ID, Currency: "KES"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Move(ctx, tid, d.ID, s1.ID); err != nil { // same stage: no move
		t.Fatal(err)
	}
	if n := len(stageEvents(t, db, d.ID)); n != 0 {
		t.Fatalf("a move to the same stage emitted %d events", n)
	}
	if _, err := svc.Move(ctx, tid, d.ID, s2.ID); err != nil {
		t.Fatal(err)
	}
	title := "Order (renamed)"
	if _, err := svc.Update(ctx, tid, d.ID, UpdateInput{Title: &title}); err != nil { // no stage change
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, tid, d.ID, UpdateInput{StageID: &s1.ID, Title: &title}); err != nil {
		t.Fatal(err)
	}
	evs := stageEvents(t, db, d.ID)
	if len(evs) != 2 {
		t.Fatalf("want 2 stage events, got %d", len(evs))
	}
	moves := map[string]bool{}
	for _, e := range evs {
		moves[e["move_id"].(string)] = true
		if e["deal_id"] != d.ID.String() {
			t.Fatalf("event = %+v", e)
		}
	}
	if len(moves) != 2 {
		t.Fatal("each move needs its own move_id")
	}
	got, _ := db.Deal.Get(ctx, d.ID)
	if got.StageID != s1.ID || got.Title != title {
		t.Fatalf("update with a stage change lost a field: %+v", got)
	}
}
