package custom_agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// allowAutopilotPlan is a plan that includes Autopilot with limit agents (-1 unlimited).
func allowAutopilotPlan(limit int) func(*Service) {
	return WithAutopilotPlan(
		func(context.Context, string, string) (bool, bool) { return true, true },
		func(context.Context, string, string) (int, bool, bool) { return limit, true, true })
}

func openPlanTestDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testCustomAgentsPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// planTenant creates a tenant with the given slug prefix, cleaned up after the test.
func planTenant(t *testing.T, db *ent.Client, slug string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Plan " + tid.String()).SetSlug(slug).ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	return tid
}

func plainAgent(t *testing.T, db *ent.Client, tid uuid.UUID, name string) *ent.CustomAgent {
	t.Helper()
	return db.CustomAgent.Create().SetTenantID(tid).SetName(name).SetTriggerType("manual").SaveX(context.Background())
}

var autopilotOffers = []map[string]any{{"name": "Blender"}}

// Owner 2026-11-14: an Autopilot agent needs marketflow_autopilot (strict: an unknown answer
// refuses, an unwired plan refuses), and switching one on needs a slot under max_autopilot_agents.
func TestAutopilotPlanGate(t *testing.T) {
	db := openPlanTestDB(t)
	ctx := context.Background()
	on := true
	off := false

	t.Run("not in the plan", func(t *testing.T) {
		tid := planTenant(t, db, "plan-none-"+uuid.NewString())
		a := plainAgent(t, db, tid, "Ads")
		without := NewService(db, zap.NewNop(), WithAutopilotPlan(
			func(context.Context, string, string) (bool, bool) { return false, true },
			func(context.Context, string, string) (int, bool, bool) { return 0, false, true }))
		if _, err := without.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: okConfig(), Offerings: autopilotOffers}); !errors.Is(err, ErrAutopilotNotInPlan) {
			t.Fatalf("a plan without Autopilot: %v", err)
		}
		if got := db.CustomAgent.GetX(ctx, a.ID); isAutopilotAgent(got) {
			t.Fatal("a refused save must not turn the agent into an autopilot")
		}
		unwired := NewService(db, zap.NewNop())
		if _, err := unwired.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: okConfig(), Offerings: autopilotOffers}); !errors.Is(err, ErrAutopilotNotInPlan) {
			t.Fatalf("no plan check wired must refuse (fail closed): %v", err)
		}
		st, err := without.AutopilotPlanStatus(ctx, tid)
		if err != nil || st.Included || st.CanActivate || st.UpgradePath != AutopilotUpgradePath {
			t.Fatalf("status without the feature: %+v %v", st, err)
		}
	})

	t.Run("plan unknown", func(t *testing.T) {
		tid := planTenant(t, db, "plan-unknown-"+uuid.NewString())
		a := plainAgent(t, db, tid, "Ads")
		outage := NewService(db, zap.NewNop(), WithAutopilotPlan(
			func(context.Context, string, string) (bool, bool) { return false, false },
			func(context.Context, string, string) (int, bool, bool) { return 0, false, false }))
		if _, err := outage.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: okConfig(), Offerings: autopilotOffers}); !errors.Is(err, ErrAutopilotPlanUnknown) {
			t.Fatalf("an outage refuses: %v", err)
		}
		if st, _ := outage.AutopilotPlanStatus(ctx, tid); !st.Unknown || st.Included {
			t.Fatalf("status during an outage: %+v", st)
		}
	})

	t.Run("limit", func(t *testing.T) {
		tid := planTenant(t, db, "plan-one-"+uuid.NewString())
		s := NewService(db, zap.NewNop(), allowAutopilotPlan(1))
		first, second := plainAgent(t, db, tid, "First"), plainAgent(t, db, tid, "Second")
		for _, a := range []*ent.CustomAgent{first, second} {
			if _, err := s.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: okConfig(), Offerings: autopilotOffers}); err != nil {
				t.Fatalf("saving a switched-off autopilot takes no slot: %v", err)
			}
		}
		if _, err := s.Update(ctx, tid, first.ID, UpdateInput{IsActive: &on}); err != nil {
			t.Fatalf("the first agent fits: %v", err)
		}
		_, err := s.Update(ctx, tid, second.ID, UpdateInput{IsActive: &on})
		var lim *AutopilotLimitError
		if !errors.As(err, &lim) || !errors.Is(err, ErrAutopilotLimit) || lim.Limit != 1 || lim.Active != 1 {
			t.Fatalf("the second agent is over the limit: %v", err)
		}
		if db.CustomAgent.GetX(ctx, second.ID).IsActive {
			t.Fatal("a refused switch-on must leave the agent off")
		}
		st, err := s.AutopilotPlanStatus(ctx, tid)
		if err != nil || !st.Included || st.Limit != 1 || st.Active != 1 || st.CanActivate {
			t.Fatalf("status at the limit: %+v %v", st, err)
		}
		// Saving the active agent again keeps its slot, it does not count against itself.
		if _, err := s.SaveAutopilot(ctx, tid, first.ID, AutopilotInput{Config: okConfig(), Offerings: autopilotOffers}); err != nil {
			t.Fatalf("re-saving the active autopilot: %v", err)
		}
		// A switched-on plain agent turning into an autopilot takes a slot too.
		plain := plainAgent(t, db, tid, "Plain")
		db.CustomAgent.UpdateOneID(plain.ID).SetIsActive(true).ExecX(ctx)
		if _, err := s.SaveAutopilot(ctx, tid, plain.ID, AutopilotInput{Config: okConfig(), Offerings: autopilotOffers}); !errors.Is(err, ErrAutopilotLimit) {
			t.Fatalf("an active agent becoming an autopilot over the limit: %v", err)
		}
		// Switching the first off frees the slot.
		if _, err := s.Update(ctx, tid, first.ID, UpdateInput{IsActive: &off}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Update(ctx, tid, second.ID, UpdateInput{IsActive: &on}); err != nil {
			t.Fatalf("a freed slot: %v", err)
		}
		// Plain agents never count and are never gated.
		other := plainAgent(t, db, tid, "Other")
		if _, err := NewService(db, zap.NewNop()).Update(ctx, tid, other.ID, UpdateInput{IsActive: &on}); err != nil {
			t.Fatalf("a plain agent is not an autopilot: %v", err)
		}
	})

	t.Run("unlimited and default", func(t *testing.T) {
		tid := planTenant(t, db, "plan-many-"+uuid.NewString())
		s := NewService(db, zap.NewNop(), allowAutopilotPlan(-1))
		for i := 0; i < 3; i++ {
			a := plainAgent(t, db, tid, "Ads")
			if _, err := s.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: okConfig(), Offerings: autopilotOffers}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Update(ctx, tid, a.ID, UpdateInput{IsActive: &on}); err != nil {
				t.Fatalf("-1 is unlimited: %v", err)
			}
		}
		if st, _ := s.AutopilotPlanStatus(ctx, tid); st.Limit != -1 || st.Active != 3 || !st.CanActivate {
			t.Fatalf("unlimited status: %+v", st)
		}
		// A plan with the feature but no limit key gets the smallest allowance, one agent.
		noKey := NewService(db, zap.NewNop(), WithAutopilotPlan(
			func(context.Context, string, string) (bool, bool) { return true, true },
			func(context.Context, string, string) (int, bool, bool) { return 0, false, true }))
		if st, _ := noKey.AutopilotPlanStatus(ctx, tid); st.Limit != defaultAutopilotAgents || st.CanActivate {
			t.Fatalf("no limit key: %+v", st)
		}
	})

	t.Run("platform tenant exempt", func(t *testing.T) {
		slug := "plan-platform-" + uuid.NewString()
		tid := planTenant(t, db, slug)
		s := NewService(db, zap.NewNop(), WithPlatformTenant(slug)) // no plan check wired at all
		a := plainAgent(t, db, tid, "Ads")
		if _, err := s.SaveAutopilot(ctx, tid, a.ID, AutopilotInput{Config: okConfig(), Offerings: autopilotOffers}); err != nil {
			t.Fatalf("the platform tenant is exempt: %v", err)
		}
		if _, err := s.Update(ctx, tid, a.ID, UpdateInput{IsActive: &on}); err != nil {
			t.Fatalf("the platform tenant is exempt from the limit: %v", err)
		}
		if st, _ := s.AutopilotPlanStatus(ctx, tid); !st.Exempt || !st.Included || st.Limit != -1 {
			t.Fatalf("platform status: %+v", st)
		}
	})

	t.Run("hire needs the plan", func(t *testing.T) {
		slug := "plan-store-" + uuid.NewString()
		pid := planTenant(t, db, slug)
		tid := planTenant(t, db, "plan-hire-"+uuid.NewString())
		tpl := db.CustomAgent.Create().SetTenantID(pid).SetName("Weekly ads").SetTriggerType("cron").
			SetMetadata(map[string]any{"kind": KindAdAutopilot, "template": map[string]any{"category": "ads", "version": 1}}).SaveX(ctx)
		without := NewService(db, zap.NewNop(), WithPlatformTenant(slug), WithAutopilotPlan(
			func(context.Context, string, string) (bool, bool) { return false, true }, nil))
		if _, err := without.Hire(ctx, tid, tpl.ID); !errors.Is(err, ErrAutopilotNotInPlan) {
			t.Fatalf("hiring an Autopilot template without the plan: %v", err)
		}
		with := NewService(db, zap.NewNop(), WithPlatformTenant(slug), allowAutopilotPlan(1))
		if _, err := with.Hire(ctx, tid, tpl.ID); err != nil {
			t.Fatalf("hiring with the plan: %v", err)
		}
	})
}

// The UI reads code, limit and upgrade_path from the error body to show the upgrade hint.
func TestAutopilotPlanReply(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{&AutopilotLimitError{Limit: 3, Active: 3}, http.StatusPaymentRequired, "autopilot_limit_reached"},
		{ErrAutopilotNotInPlan, http.StatusForbidden, "autopilot_not_in_plan"},
		{ErrAutopilotPlanUnknown, http.StatusServiceUnavailable, "plan_unavailable"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		if !autopilotPlanReply(w, c.err) {
			t.Fatalf("%v: not answered", c.err)
		}
		var body struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != c.status || body.Code != c.code || body.Details["upgrade_path"] != AutopilotUpgradePath {
			t.Errorf("%v: %d %+v", c.err, w.Code, body)
		}
		if c.code == "autopilot_limit_reached" && body.Details["limit"] != float64(3) {
			t.Errorf("limit detail: %+v", body.Details)
		}
	}
	if autopilotPlanReply(httptest.NewRecorder(), errors.New("other")) {
		t.Fatal("another error is not a plan error")
	}
}
