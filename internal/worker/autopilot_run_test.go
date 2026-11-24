package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/modules/publishing/postingplan"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func seedAutopilot(t *testing.T, db *ent.Client, tid uuid.UUID, credits int) *ent.CustomAgent {
	t.Helper()
	return db.CustomAgent.Create().SetTenantID(tid).SetName("Weekly ads").SetTriggerType(custom_agents.TriggerSchedule).
		SetIsActive(true).SetMetadata(map[string]any{
		"kind": custom_agents.KindAdAutopilot,
		"autopilot": map[string]any{"offering_ids": []string{"o1", "o2"}, "objective": "whatsapp", "cadence": "weekly", "weekday": 1, "time": "09:00",
			"platforms": []string{"facebook"}, "connected_account_ids": []string{"a1"}, "monthly_credit_ceiling": credits},
		"autopilot_offerings": []map[string]any{
			{"name": "Blender", "photo_urls": []string{"https://media.example/blender.jpg"}},
			{"name": "Kettle", "photo_urls": []string{"https://media.example/kettle.jpg"}},
		},
	}).SaveX(context.Background())
}

// A short balance skips the run before anything is made and can be told apart from a reached
// ceiling; a forecast that cannot be read never blocks the run.
func TestAutopilotRunCreditForecast(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "autopilot-forecast")
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	r := NewAutopilotRunner(db, nil, "https://mf.example", zap.NewNop())
	a := seedAutopilot(t, db, tid, 100)

	r.forecast = func(context.Context, uuid.UUID) (custom_agents.CreditForecast, error) {
		return custom_agents.CreditForecast{CreditsNeeded: 2, Balance: 1}, nil
	}
	if _, err := r.Run(ctx, a); !errors.Is(err, errAutopilotNoCredits) || !errors.Is(err, ErrAutopilotSkipped) {
		t.Fatalf("a short balance must skip: %v", err)
	}
	if n := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).CountX(ctx); n != 0 {
		t.Fatalf("a skipped run made %d briefs", n)
	}

	r.forecast = func(context.Context, uuid.UUID) (custom_agents.CreditForecast, error) {
		return custom_agents.CreditForecast{CreditsNeeded: 101, Balance: 500, Enough: true}, nil
	}
	if _, err := r.Run(ctx, a); !errors.Is(err, ErrAutopilotSkipped) || errors.Is(err, errAutopilotNoCredits) {
		t.Fatalf("a draft past the ceiling is a plain skip: %v", err)
	}

	r.forecast = func(context.Context, uuid.UUID) (custom_agents.CreditForecast, error) {
		return custom_agents.CreditForecast{}, errors.New("marketflow-ai is down")
	}
	if res, err := r.Run(ctx, a); err != nil || res.Offering != "Blender" {
		t.Fatalf("an unreadable forecast must not block: %+v %v", res, err)
	}
}

// A run makes a draft for review and never approves it; finished runs rotate the offering; a
// reached credit ceiling skips the run.
func TestAutopilotRunDraftsAndRotates(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "autopilot")
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	r := NewAutopilotRunner(db, nil, "https://mf.example", zap.NewNop())
	a := seedAutopilot(t, db, tid, 100)

	first, err := r.Run(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if first.Offering != "Blender" || first.Run != 0 {
		t.Fatalf("first run: %+v", first)
	}
	vid := uuid.MustParse(first.VersionID)
	v := db.CreativeVersion.GetX(ctx, vid)
	if v.Status == "approved" || v.ApprovedBy != nil {
		t.Fatalf("an agent never approves its own work: %+v", v)
	}
	b := db.CreativeBrief.GetX(ctx, uuid.MustParse(first.BriefID))
	if b.Metadata["autopilot_agent_id"] != a.ID.String() {
		t.Fatalf("the brief must name its agent: %v", b.Metadata)
	}

	db.AgentRun.Create().SetTenantID(tid).SetCustomAgentID(a.ID).SetTriggeredBy("schedule").SetStatus("completed").ExecX(ctx)
	second, err := r.Run(ctx, a)
	if err != nil || second.Offering != "Kettle" || second.Run != 1 {
		t.Fatalf("the rotation moves on after a finished run: %+v %v", second, err)
	}

	// Spend reaches the ceiling: the next run is skipped, not failed.
	db.CreativeVersion.UpdateOneID(vid).SetCredits(100).ExecX(ctx)
	if _, err := r.Run(ctx, a); !errors.Is(err, ErrAutopilotSkipped) {
		t.Fatalf("over the ceiling must skip: %v", err)
	}
}

// A veto-window agent's draft is marked for the autopublish pipeline with its run, but only while
// the plan and the terms allow it; otherwise the run is approve-each and the brief says why.
func TestAutopilotRunVetoWindow(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "autopilot-veto")
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{custom_agents.TermsKey: map[string]any{"version": "v1"}}).ExecX(ctx)
	a := seedAutopilot(t, db, tid, 100)
	meta := a.Metadata
	cfg := meta["autopilot"].(map[string]any)
	cfg["approval_mode"], cfg["veto_hours"] = custom_agents.AutoPublish, 4
	a = db.CustomAgent.UpdateOneID(a.ID).SetMetadata(meta).SaveX(ctx)
	runID := uuid.New()

	allowed := NewAutopilotRunner(db, nil, "https://mf.example", zap.NewNop()).WithAutoPublishGate(custom_agents.AutoPublishGate{
		DB: db, HasFeature: func(context.Context, string, string) bool { return true }, TermsVersion: "v1"})
	res, err := allowed.RunWithID(ctx, a, runID)
	if err != nil || !res.VetoWindow {
		t.Fatalf("veto-window run: %+v %v", res, err)
	}
	v := db.CreativeVersion.GetX(ctx, uuid.MustParse(res.VersionID))
	ap, _ := v.Metadata["autopilot"].(map[string]any)
	if ap["agent_id"] != a.ID.String() || ap["run_id"] != runID.String() || ap["state"] != "active" || v.Status == "approved" {
		t.Fatalf("the draft must carry the agent and the run: %v", v.Metadata)
	}
	if b := db.CreativeBrief.GetX(ctx, uuid.MustParse(res.BriefID)); b.Metadata["autopilot_run_id"] != runID.String() {
		t.Fatalf("the brief names the run: %v", b.Metadata)
	}

	// Owner 2026-11-14: the failed gate also tells the pipeline to cancel the posts already waiting.
	hook := &AutopilotLapsedHook{}
	var told []uuid.UUID
	hook.Set(func(_ context.Context, tenantID uuid.UUID) { told = append(told, tenantID) })
	lapsed := NewAutopilotRunner(db, nil, "https://mf.example", zap.NewNop()).WithAutoPublishGate(custom_agents.AutoPublishGate{
		DB: db, HasFeature: func(context.Context, string, string) bool { return false }, TermsVersion: "v1"}).WithLapsedHook(hook)
	res, err = lapsed.RunWithID(ctx, a, uuid.New())
	if err != nil || res.VetoWindow {
		t.Fatalf("a lapsed plan runs approve-each: %+v %v", res, err)
	}
	if len(told) != 1 || told[0] != tid {
		t.Fatalf("the pipeline must be told to cancel the waiting posts: %v", told)
	}
	if v := db.CreativeVersion.GetX(ctx, uuid.MustParse(res.VersionID)); v.Metadata["autopilot"] != nil {
		t.Fatalf("an approve-each draft is not the pipeline's: %v", v.Metadata)
	}
	if b := db.CreativeBrief.GetX(ctx, uuid.MustParse(res.BriefID)); b.Metadata["approval_mode"] != custom_agents.ApproveEach || b.Metadata["veto_fallback"] == "" {
		t.Fatalf("the brief records the fallback: %v", b.Metadata)
	}
}

// An agent that follows the posting cadence makes nothing once every platform has its posts
// (here a cadence of none); with posts still due it runs as usual.
func TestAutopilotRunFollowsCadence(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "autopilot-cadence")
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	r := NewAutopilotRunner(db, nil, "https://mf.example", zap.NewNop())
	a := seedAutopilot(t, db, tid, 100)
	a.Metadata["autopilot"].(map[string]any)["follow_cadence"] = true
	a = db.CustomAgent.UpdateOne(a).SetMetadata(a.Metadata).SaveX(ctx)

	setCadence := func(n int) {
		if err := metastore.SetKeys(ctx, db, tid, map[string]any{
			postingplan.TenantCadenceKey: map[string]any{"facebook": map[string]any{"count": n, "per": "week"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	setCadence(0)
	if _, err := r.Run(ctx, a); !errors.Is(err, errAutopilotCadenceMet) || !errors.Is(err, ErrAutopilotSkipped) {
		t.Fatalf("no post due must skip: %v", err)
	}
	if n := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).CountX(ctx); n != 0 {
		t.Fatalf("a skipped run made %d briefs", n)
	}
	setCadence(3)
	if res, err := r.Run(ctx, a); err != nil || res.Offering != "Blender" {
		t.Fatalf("a due platform runs: %+v %v", res, err)
	}
}
