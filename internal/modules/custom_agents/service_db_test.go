package custom_agents

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
)

const testCustomAgentsPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

// Team-member overrides share the table but must never surface through the
// custom agents API, while agents with NULL or kind-less metadata still do.
func TestCustomAgentsHideTeamMemberRows(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testCustomAgentsPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("CA " + tid.String()).SetSlug("ca-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})

	plain, err := db.CustomAgent.Create().SetTenantID(tid).SetName("plain").SetTriggerType("manual").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CustomAgent.Create().SetTenantID(tid).SetName("other").SetTriggerType("manual").
		SetMetadata(map[string]any{"source": "ui"}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	team, err := db.CustomAgent.Create().SetTenantID(tid).SetName("Copywriter").SetTriggerType("manual").
		SetMetadata(map[string]any{"kind": "team_member", "key": "copywriter"}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	svc := NewService(db, zap.NewNop())
	list, err := svc.List(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != plain.ID || list[1].ID != other.ID {
		t.Fatalf("list = %d rows, want the plain and other agents only", len(list))
	}
	if _, err := svc.Get(ctx, tid, team.ID); err == nil {
		t.Fatal("Get returned a team member row")
	}
	if _, err := svc.TriggerRun(ctx, tid, team.ID, "manual", nil); err == nil {
		t.Fatal("TriggerRun ran a team member row")
	}
	if err := svc.Delete(ctx, tid, team.ID); err == nil {
		t.Fatal("Delete removed a team member row")
	}

	// An empty tool list clears the tools; an absent one leaves them alone.
	if _, err := svc.Update(ctx, tid, plain.ID, UpdateInput{AllowedTools: []string{"kb_search", "create_task"}}); err != nil {
		t.Fatal(err)
	}
	name := "renamed"
	if got, _ := svc.Update(ctx, tid, plain.ID, UpdateInput{Name: &name}); len(got.AllowedTools) != 2 {
		t.Fatalf("absent tools changed the list: %v", got.AllowedTools)
	}
	if got, _ := svc.Update(ctx, tid, plain.ID, UpdateInput{AllowedTools: []string{}}); len(got.AllowedTools) != 0 {
		t.Fatalf("empty tools did not clear: %v", got.AllowedTools)
	}
}
