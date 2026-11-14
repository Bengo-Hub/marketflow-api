package autopublish

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/config"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/ent/auditlog"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"
)

const testPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fixture struct {
	db    *ent.Client
	p     *Pipeline
	tid   uuid.UUID
	agent *ent.CustomAgent
	run   *ent.AgentRun
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := openDB(t)
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Shea Co").SetSlug("veto-" + tid.String()).
		SetMetadata(map[string]any{custom_agents.TermsKey: map[string]any{"version": "v1"}}).ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.AuditLog.Delete().Where(auditlog.TenantID(tid)).Exec(ctx)
		_, _ = db.PublishJob.Delete().Where(publishjob.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeAsset.Delete().Where(creativeasset.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.AgentRun.Delete().Where(agentrun.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	cfg := map[string]any{"offering_ids": []string{"o1"}, "objective": "whatsapp", "cadence": "weekly", "weekday": 1, "time": "09:00",
		"platforms": []string{"facebook"}, "connected_account_ids": []string{uuid.NewString()}, "monthly_credit_ceiling": 200,
		"approval_mode": custom_agents.AutoPublish, "veto_hours": 4}
	agent := db.CustomAgent.Create().SetTenantID(tid).SetName("Weekly ads").SetTriggerType(custom_agents.TriggerSchedule).SetIsActive(true).
		SetMetadata(map[string]any{"kind": custom_agents.KindAdAutopilot, "autopilot": cfg}).SaveX(ctx)
	run := db.AgentRun.Create().SetTenantID(tid).SetCustomAgentID(agent.ID).SetTriggeredBy("manual").SetStatus("completed").
		SetTrace(map[string]any{}).SaveX(ctx)
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	p := New(Deps{DB: db, Studio: creative.NewService(db, zap.NewNop()), Publish: &publishing.EntStore{DB: db},
		Gate:   custom_agents.AutoPublishGate{DB: db, HasFeature: func(context.Context, string, string) bool { return true }, TermsVersion: "v1"},
		AppURL: "https://mf.example", Config: config.AutopilotConfig{VetoLinkGrace: time.Hour}, Logger: zap.NewNop()})
	p.now = func() time.Time { return now }
	return &fixture{db: db, p: p, tid: tid, agent: agent, run: run, now: now}
}

// video makes a finished autopilot video with the given state and one job per job status.
func (f *fixture) video(t *testing.T, state string, publishAt time.Time, jobStatuses ...string) (*ent.CreativeVersion, []*ent.PublishJob) {
	t.Helper()
	ctx := context.Background()
	title := "Shea butter (autopilot)"
	b, err := f.p.Studio.CreateBrief(ctx, f.tid, nil, creative.BriefInput{Title: &title,
		Offering: map[string]any{"name": "Shea butter", "photo_urls": []any{"https://example.com/shea.jpg"}},
		Metadata: map[string]any{"autopilot_agent_id": f.agent.ID.String()}})
	if err != nil {
		t.Fatal(err)
	}
	v := f.db.CreativeVersion.Create().SetTenantID(f.tid).SetBriefID(b.ID).SetVersionNo(1).SetKind(creative.KindVideoAd).
		SetFormat("1080x1920").SetStatus(creative.StatusReady).
		SetCopy(map[string]any{"end_card": map[string]any{"headline": "Shea butter", "cta": "Order now"}}).
		SaveX(ctx)
	asset := f.db.CreativeAsset.Create().SetTenantID(f.tid).SetVersionID(v.ID).SetKind(creative.AssetVideo).
		SetStorageKey("k/" + v.ID.String()).SetURL("https://media.example.com/v.mp4").SetExpiresAt(f.now.Add(time.Hour)).SaveX(ctx)
	var jobs []*ent.PublishJob
	var ids []any
	for _, s := range jobStatuses {
		j := f.db.PublishJob.Create().SetTenantID(f.tid).SetAssetID(asset.ID).SetConnectedAccountID(uuid.New()).
			SetPlatform(publishing.Facebook).SetStatus(s).SetScheduledAt(publishAt).SetDedupeKey(uuid.NewString()).
			SetMetadata(map[string]any{"planned_at": publishAt.Format(time.RFC3339)}).SaveX(ctx)
		jobs, ids = append(jobs, j), append(ids, j.ID.String())
	}
	v = f.db.CreativeVersion.UpdateOneID(v.ID).SetMetadata(map[string]any{creative.MetaAutopilot: map[string]any{
		"agent_id": f.agent.ID.String(), "run_id": f.run.ID.String(), "state": state, "veto_hours": 4,
		"publish_at": publishAt.Format(time.RFC3339), "job_ids": ids,
	}}).SaveX(ctx)
	return v, jobs
}

// A person's veto cancels the waiting posts, frees their dedupe keys for a later manual post and
// is recorded apart from the agent's steps; a second veto is a no-op.
func TestVetoCancelsTheScheduledPost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, jobs := f.video(t, StateScheduled, f.now.Add(4*time.Hour), publishing.StatusPending, publishing.StatusPending)

	res, err := f.p.Veto(ctx, f.tid, v.ID, "user:u1")
	if err != nil || res.Cancelled != 2 || res.State != StateVetoed {
		t.Fatalf("veto: %+v %v", res, err)
	}
	for _, j := range jobs {
		got := f.db.PublishJob.GetX(ctx, j.ID)
		if got.Status != publishing.StatusCancelled || got.DedupeKey != "vetoed:"+j.ID.String() {
			t.Fatalf("job not cancelled or its key not freed: %+v", got)
		}
	}
	st := readState(f.db.CreativeVersion.GetX(ctx, v.ID))
	if st.State != StateVetoed || st.Raw["decision"].(map[string]any)["by"] != "user:u1" {
		t.Fatalf("state: %+v", st.Raw)
	}
	if n := f.db.AuditLog.Query().Where(auditlog.TenantID(f.tid), auditlog.Event("autopilot.post_vetoed"), auditlog.Actor("user:u1")).CountX(ctx); n != 1 {
		t.Fatalf("audit rows: %d", n)
	}
	trace := f.db.AgentRun.GetX(ctx, f.run.ID).Trace
	if steps, _ := trace["veto_window"].([]any); len(steps) != 1 {
		t.Fatalf("the run's trace: %v", trace)
	}
	again, err := f.p.Veto(ctx, f.tid, v.ID, "user:u1")
	if err != nil || again.State != StateVetoed {
		t.Fatalf("second veto: %+v %v", again, err)
	}
	if _, err := f.p.Veto(ctx, uuid.New(), v.ID, "user:u1"); !ent.IsNotFound(err) {
		t.Fatalf("another tenant must not reach this video: %v", err)
	}
}

// A veto after the post reached the platform answers too late; when the window ends with no veto
// the video is approved by the window, not by a person, and the post goes ahead.
func TestVetoTooLateAndRelease(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	at := f.now.Add(-time.Minute)
	v, _ := f.video(t, StateScheduled, at, publishing.StatusUploading)

	res, err := f.p.Veto(ctx, f.tid, v.ID, ActorVetoLink)
	if !errors.Is(err, ErrTooLate) || len(res.Gone) != 1 {
		t.Fatalf("too late: %+v %v", res, err)
	}
	if err := f.p.Advance(ctx, f.tid, v.ID); err != nil {
		t.Fatal(err)
	}
	got := f.db.CreativeVersion.GetX(ctx, v.ID)
	st := readState(got)
	if got.Status != creative.StatusApproved || got.ApprovedBy != nil || st.State != StateReleased ||
		st.Raw["decision"].(map[string]any)["by"] != ActorVetoWindow {
		t.Fatalf("release: %s %v", got.Status, st.Raw)
	}
	if ev := got.Metadata["approval"].(map[string]any); ev["channel"] != string(creative.ApprovalVetoWindow) {
		t.Fatalf("approval evidence: %v", ev)
	}
	if _, err := f.p.Veto(ctx, f.tid, v.ID, "user:u1"); !errors.Is(err, ErrTooLate) {
		t.Fatalf("a veto after the release: %v", err)
	}
}

// Owner 2026-11-14: an agent switched off while its post waits in the window has that post
// cancelled at once, recorded as cancelled because the agent was turned off.
func TestAgentOffCancelsWaitingPosts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, jobs := f.video(t, StateScheduled, f.now.Add(2*time.Hour), publishing.StatusPending)
	f.db.CustomAgent.UpdateOneID(f.agent.ID).SetIsActive(false).ExecX(ctx)

	f.p.AgentChanged(ctx, f.tid, f.agent.ID)
	if got := f.db.PublishJob.GetX(ctx, jobs[0].ID); got.Status != publishing.StatusCancelled {
		t.Fatalf("the waiting post must be cancelled: %s", got.Status)
	}
	st := readState(f.db.CreativeVersion.GetX(ctx, v.ID))
	d, _ := st.Raw["decision"].(map[string]any)
	if st.State != StateVetoed || d["by"] != ActorAgentOff || d["note"] != "the agent was switched off" {
		t.Fatalf("state: %v", st.Raw)
	}
	if n := f.db.AuditLog.Query().Where(auditlog.TenantID(f.tid), auditlog.Actor(ActorAgentOff)).CountX(ctx); n != 1 {
		t.Fatalf("audit rows: %d", n)
	}
}

// With nowhere to post (here: no way to send the heads-up) a finished video waits for a person,
// recorded as the agent's step; a video discarded during its window has its posts cancelled.
func TestHoldAndDiscardDuringWindow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ready, _ := f.video(t, StateActive, time.Time{})
	if err := f.p.Advance(ctx, f.tid, ready.ID); err != nil {
		t.Fatal(err)
	}
	st := readState(f.db.CreativeVersion.GetX(ctx, ready.ID))
	if st.State != StateHeld || st.Reason == "" {
		t.Fatalf("hold: %v", st.Raw)
	}
	if n := f.db.AuditLog.Query().Where(auditlog.TenantID(f.tid), auditlog.Event("autopilot.held"), auditlog.Actor("agent:"+f.agent.ID.String())).CountX(ctx); n != 1 {
		t.Fatalf("the hold is the agent's step: %d", n)
	}

	v, jobs := f.video(t, StateScheduled, f.now.Add(time.Hour), publishing.StatusPending)
	f.db.CreativeVersion.UpdateOneID(v.ID).SetStatus(creative.StatusRejected).ExecX(ctx)
	if err := f.p.Advance(ctx, f.tid, v.ID); err != nil {
		t.Fatal(err)
	}
	if f.db.PublishJob.GetX(ctx, jobs[0].ID).Status != publishing.StatusCancelled {
		t.Fatal("a discarded video's post must not go out")
	}
	if st := readState(f.db.CreativeVersion.GetX(ctx, v.ID)); st.State != StateVetoed {
		t.Fatalf("discarded: %v", st.Raw)
	}
}
