package custom_agents

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Saving makes the agent an ad autopilot on the config's schedule and keeps a version history;
// rollback restores an old config as a new version; auto-publish (the veto window) needs the plan
// feature and the current terms; template_ref stays.
func TestSaveAndRollbackAutopilot(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testCustomAgentsPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("AP " + tid.String()).SetSlug("ap-" + tid.String()).ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	a := db.CustomAgent.Create().SetTenantID(tid).SetName("Ads").SetTriggerType("manual").
		SetMetadata(map[string]any{"template_ref": map[string]any{"id": "t1"}}).SaveX(ctx)
	s := NewService(db, zap.NewNop())

	cfg := okConfig()
	offers := []map[string]any{{"name": "Blender"}}
	saved, err := s.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: cfg, Offerings: offers})
	if err != nil {
		t.Fatal(err)
	}
	if saved.TriggerType != TriggerSchedule || saved.Metadata["kind"] != KindAdAutopilot || saved.Metadata["template_ref"] == nil {
		t.Fatalf("saved: %+v", saved)
	}
	if _, err := ParseSchedule(saved.TriggerConfig); err != nil {
		t.Fatalf("the agent's schedule must parse for the scheduler: %v", err)
	}

	cfg.CreditCap = 999
	second, err := s.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: cfg, Offerings: offers})
	if err != nil || len(readVersions(second)) != 2 {
		t.Fatalf("second version: %v %v", readVersions(second), err)
	}
	back, err := s.RollbackAutopilot(ctx, tid, a.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	vs := readVersions(back)
	if len(vs) != 3 || vs[2].Version != 3 || vs[2].Config.CreditCap != okConfig().CreditCap {
		t.Fatalf("rollback adds a version with the old config: %+v", vs)
	}
	if _, err := s.RollbackAutopilot(ctx, tid, a.ID, 99); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown version: %v", err)
	}

	cfg.Approval = AutoPublish
	if _, err := s.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: cfg, Offerings: offers}); !errors.Is(err, ErrAutoPublishNotAllowed) {
		t.Fatalf("auto-publish without the plan feature gate is refused: %v", err)
	}
	// With the plan feature, the veto window still needs the current terms.
	gated := NewService(db, zap.NewNop(), WithAutoPublishGate(func(context.Context, string, string) bool { return true }, "v-test"))
	if _, err := gated.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: cfg, Offerings: offers}); !errors.Is(err, ErrAutoPublishNotAllowed) {
		t.Fatalf("auto-publish before the terms are accepted: %v", err)
	}
	by := uuid.New()
	st, err := gated.SetAutoPublish(ctx, tid, &by, AutoPublishInput{AcceptTerms: true, HoldCategories: []string{" Alcohol ", "alcohol", ""}})
	if err != nil || !st.CanAutoPublish || st.TermsVersion != "v-test" || len(st.HoldCategories) != 1 || st.HoldCategories[0] != "alcohol" {
		t.Fatalf("accept terms: %+v %v", st, err)
	}
	vetoed, err := gated.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: cfg, Offerings: offers})
	if err != nil {
		t.Fatalf("auto-publish with the plan and the terms: %v", err)
	}
	if saved, _, _ := ReadAutopilot(vetoed); saved.Approval != AutoPublish || saved.VetoHours != DefaultVeto {
		t.Fatalf("the veto window gets the default: %+v", saved)
	}
	// A new terms version asks again; a lapsed plan refuses (fail closed).
	newer := NewService(db, zap.NewNop(), WithAutoPublishGate(func(context.Context, string, string) bool { return true }, "v-next"))
	if _, err := newer.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: cfg, Offerings: offers}); !errors.Is(err, ErrAutoPublishNotAllowed) {
		t.Fatalf("a new terms version must be accepted again: %v", err)
	}
	lapsed := NewService(db, zap.NewNop(), WithAutoPublishGate(func(context.Context, string, string) bool { return false }, "v-test"))
	if _, err := lapsed.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: cfg, Offerings: offers}); !errors.Is(err, ErrAutoPublishNotAllowed) {
		t.Fatalf("without the plan feature: %v", err)
	}
	if _, err := s.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: okConfig(), Offerings: nil}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing offering details: %v", err)
	}
	if _, err := s.SaveAutopilot(ctx, uuid.New(), a.ID, AutopilotInput{Config: okConfig(), Offerings: offers}); !ent.IsNotFound(err) {
		t.Fatalf("another tenant must not reach this agent: %v", err)
	}
}
