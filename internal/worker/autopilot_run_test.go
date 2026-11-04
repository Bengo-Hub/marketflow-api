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
