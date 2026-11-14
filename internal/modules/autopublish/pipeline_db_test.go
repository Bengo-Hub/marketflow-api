package autopublish

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
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

// Owner 2026-11-14: a plan that no longer includes auto-publish cancels the posts already waiting
// at once, like switching the agent off, with its own reason and actor.
func TestPlanLapseCancelsWaitingPosts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, jobs := f.video(t, StateScheduled, f.now.Add(2*time.Hour), publishing.StatusPending)
	f.p.Gate.HasFeature = func(context.Context, string, string) bool { return false }

	f.p.AutoPublishLapsed(ctx, f.tid)
	if got := f.db.PublishJob.GetX(ctx, jobs[0].ID); got.Status != publishing.StatusCancelled {
		t.Fatalf("the waiting post must be cancelled: %s", got.Status)
	}
	st := readState(f.db.CreativeVersion.GetX(ctx, v.ID))
	d, _ := st.Raw["decision"].(map[string]any)
	if st.State != StateVetoed || d["by"] != ActorAutoPublishLapsed || d["note"] != WhyPlanLapsed {
		t.Fatalf("state: %v", st.Raw)
	}
	if n := f.db.AuditLog.Query().Where(auditlog.TenantID(f.tid), auditlog.Actor(ActorAutoPublishLapsed)).CountX(ctx); n != 1 {
		t.Fatalf("audit rows: %d", n)
	}
}

// Owner 2026-11-14: a new terms version applies from its effective date. Before it, the earlier
// accepted version still counts and the post waits; from it, the post is cancelled with the terms
// reason (the sweep's path: Advance on a scheduled video).
func TestNewTermsCancelFromTheirEffectiveDate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, jobs := f.video(t, StateScheduled, f.now.Add(2*time.Hour), publishing.StatusPending)
	clock := f.now
	f.p.Gate.TermsVersion = "v2" // the tenant accepted v1
	f.p.Gate.TermsEffectiveAt = f.now.Add(24 * time.Hour)
	f.p.Gate.Now = func() time.Time { return clock }

	if err := f.p.Advance(ctx, f.tid, v.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.db.PublishJob.GetX(ctx, jobs[0].ID); got.Status != publishing.StatusPending {
		t.Fatalf("before the effective date the post still waits: %s", got.Status)
	}
	if st, _ := f.p.Gate.Status(ctx, f.tid); !st.TermsAccepted || !st.NewTermsPending || st.TermsEffectiveAt == "" {
		t.Fatalf("status before the effective date: %+v", st)
	}

	clock = f.now.Add(25 * time.Hour)
	if err := f.p.Advance(ctx, f.tid, v.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.db.PublishJob.GetX(ctx, jobs[0].ID); got.Status != publishing.StatusCancelled {
		t.Fatalf("from the effective date the post is cancelled: %s", got.Status)
	}
	st := readState(f.db.CreativeVersion.GetX(ctx, v.ID))
	d, _ := st.Raw["decision"].(map[string]any)
	if st.State != StateVetoed || d["by"] != ActorAutoPublishLapsed || d["note"] != WhyTermsChanged {
		t.Fatalf("state: %v", st.Raw)
	}
}

// Owner 2026-11-14: a plan lookup that fails (outage, timeout) never cancels. A post whose window
// ends meanwhile is not released and its job is moved on, so it waits until a check succeeds; the
// check runs once per tenant per sweep.
func TestPlanOutageNeverCancels(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v1, jobs1 := f.video(t, StateScheduled, f.now.Add(-time.Minute), publishing.StatusPending)
	v2, jobs2 := f.video(t, StateScheduled, f.now.Add(3*time.Hour), publishing.StatusPending)
	calls := 0
	f.p.Gate.CheckFeature = func(context.Context, string, string) (bool, bool) {
		calls++
		return false, false
	}

	f.p.AutoPublishLapsed(ctx, f.tid)
	if calls != 1 {
		t.Fatalf("one plan lookup per tenant per sweep, got %d", calls)
	}
	due := f.db.PublishJob.GetX(ctx, jobs1[0].ID)
	if due.Status != publishing.StatusPending || !due.ScheduledAt.After(f.now) {
		t.Fatalf("the due post waits and is moved on: %s %v", due.Status, due.ScheduledAt)
	}
	if planned, _ := due.Metadata["planned_at"].(string); planned != due.ScheduledAt.UTC().Format(time.RFC3339) {
		t.Fatalf("planned_at moves with the job, so a veto still works: %v %v", planned, due.ScheduledAt)
	}
	if later := f.db.PublishJob.GetX(ctx, jobs2[0].ID); later.Status != publishing.StatusPending || !later.ScheduledAt.Equal(jobs2[0].ScheduledAt) {
		t.Fatalf("a post far from its time is left alone: %+v", later)
	}
	for _, v := range []*ent.CreativeVersion{v1, v2} {
		if st := readState(f.db.CreativeVersion.GetX(ctx, v.ID)); st.State != StateScheduled {
			t.Fatalf("nothing is cancelled or released during an outage: %v", st.Raw)
		}
	}

	// The check succeeds again: the window that ended is released.
	f.p.Gate.CheckFeature = func(context.Context, string, string) (bool, bool) { return true, true }
	if err := f.p.Advance(ctx, f.tid, v1.ID); err != nil {
		t.Fatal(err)
	}
	if st := readState(f.db.CreativeVersion.GetX(ctx, v1.ID)); st.State != StateReleased {
		t.Fatalf("released once the check succeeds: %v", st.Raw)
	}
	// A definite "not included" still cancels.
	f.p.Gate.CheckFeature = func(context.Context, string, string) (bool, bool) { return false, true }
	if err := f.p.Advance(ctx, f.tid, v2.ID); err != nil {
		t.Fatal(err)
	}
	if st := readState(f.db.CreativeVersion.GetX(ctx, v2.ID)); st.State != StateVetoed {
		t.Fatalf("a definite lapse cancels: %v", st.Raw)
	}
}

// Owner 2026-11-14: after OutageHandover (24 h by default) of failed plan checks, counted from the
// first failed check for the post, the post is handed to the owner: its jobs are cancelled so
// nothing goes out unchecked, and it waits for approval by hand.
func TestPlanOutageHandsOverAfterADay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, jobs := f.video(t, StateScheduled, f.now.Add(-time.Minute), publishing.StatusPending)
	f.p.Gate.CheckFeature = func(context.Context, string, string) (bool, bool) { return false, false }

	if err := f.p.Advance(ctx, f.tid, v.ID); err != nil {
		t.Fatal(err)
	}
	st := readState(f.db.CreativeVersion.GetX(ctx, v.ID))
	if st.State != StateScheduled || st.Raw[planCheckFailedKey] == nil {
		t.Fatalf("the first failed check is recorded and the post waits: %v", st.Raw)
	}

	f.p.now = func() time.Time { return f.now.Add(23 * time.Hour) }
	if err := f.p.Advance(ctx, f.tid, v.ID); err != nil {
		t.Fatal(err)
	}
	if st := readState(f.db.CreativeVersion.GetX(ctx, v.ID)); st.State != StateScheduled {
		t.Fatalf("still waiting before the handover time: %v", st.Raw)
	}

	f.p.now = func() time.Time { return f.now.Add(25 * time.Hour) }
	if err := f.p.Advance(ctx, f.tid, v.ID); err != nil {
		t.Fatal(err)
	}
	got := f.db.CreativeVersion.GetX(ctx, v.ID)
	st = readState(got)
	if st.State != StateHeld || !strings.Contains(st.Reason, "could not be checked for 24 hours") || got.Status == creative.StatusApproved {
		t.Fatalf("handed to the owner, not approved: %s %v", got.Status, st.Raw)
	}
	if f.db.PublishJob.GetX(ctx, jobs[0].ID).Status != publishing.StatusCancelled {
		t.Fatal("the job is cancelled, so the runner never posts it unchecked")
	}
	if n := f.db.AuditLog.Query().Where(auditlog.TenantID(f.tid), auditlog.Event("autopilot."+StateHeld)).CountX(ctx); n != 1 {
		t.Fatalf("one hand-over: %d", n)
	}
}

// A check that succeeds again clears the first failure, so a later outage counts from its own start.
func TestPlanOutageClockResets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, _ := f.video(t, StateScheduled, f.now.Add(2*time.Hour), publishing.StatusPending)
	f.p.Gate.CheckFeature = func(context.Context, string, string) (bool, bool) { return false, false }
	if err := f.p.Advance(ctx, f.tid, v.ID); err != nil {
		t.Fatal(err)
	}
	f.p.Gate.CheckFeature = func(context.Context, string, string) (bool, bool) { return true, true }
	if err := f.p.Advance(ctx, f.tid, v.ID); err != nil {
		t.Fatal(err)
	}
	if st := readState(f.db.CreativeVersion.GetX(ctx, v.ID)); st.State != StateScheduled {
		t.Fatalf("still scheduled: %v", st.Raw)
	} else if _, seen := planCheckFailedAt(st); seen {
		t.Fatalf("the first failure is cleared: %v", st.Raw)
	}
}

// Pharmacy hand-off 2026-11-14: a pharmacy's medicine ad without its PPB approval reference is
// never scheduled or released by the window. It waits for the owner with a reason naming the
// reference, and a scheduled one has its waiting posts cancelled.
func TestPPBApprovalHoldsTheAgentsPost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.db.Tenant.UpdateOneID(f.tid).SetBusinessType(creative.BusinessTypePharmacy).ExecX(ctx)
	// A promotional advert of a medicine (a price on the end card), the kind the PPB rule holds.
	medicine := func(v *ent.CreativeVersion) {
		f.db.CreativeBrief.UpdateOneID(v.BriefID).SetOffering(map[string]any{"name": "Panadol 500mg tablets"}).ExecX(ctx)
		f.db.CreativeVersion.UpdateOneID(v.ID).SetCopy(map[string]any{"end_card": map[string]any{
			"headline": "Panadol 500mg tablets", "price": "KES 120", "cta": "Order now"}}).ExecX(ctx)
	}

	ready, _ := f.video(t, StateActive, time.Time{})
	medicine(ready)
	if err := f.p.Advance(ctx, f.tid, ready.ID); err != nil {
		t.Fatal(err)
	}
	if st := readState(f.db.CreativeVersion.GetX(ctx, ready.ID)); st.State != StateHeld || !strings.Contains(st.Reason, "Pharmacy and Poisons Board") {
		t.Fatalf("a finished medicine ad without its reference is not scheduled: %v", st.Raw)
	}

	due, jobs := f.video(t, StateScheduled, f.now.Add(-time.Minute), publishing.StatusPending)
	medicine(due)
	if err := f.p.Advance(ctx, f.tid, due.ID); err != nil {
		t.Fatal(err)
	}
	got := f.db.CreativeVersion.GetX(ctx, due.ID)
	if st := readState(got); st.State != StateHeld || !strings.Contains(st.Reason, "Panadol") || got.Status == creative.StatusApproved {
		t.Fatalf("the window never approves it: %s %v", got.Status, st.Raw)
	}
	if f.db.PublishJob.GetX(ctx, jobs[0].ID).Status != publishing.StatusCancelled {
		t.Fatal("its waiting post is cancelled")
	}
}

// Owner 2026-11-14: one account's post can be cancelled on its own. The video keeps waiting while
// another account's post remains, and is vetoed once the last one is cancelled.
func TestVetoJobCancelsOneAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, jobs := f.video(t, StateScheduled, f.now.Add(4*time.Hour), publishing.StatusPending, publishing.StatusPending)

	if _, err := f.p.VetoJob(ctx, f.tid, v.ID, uuid.New(), "user:u1"); !ent.IsNotFound(err) {
		t.Fatalf("a job of another post: %v", err)
	}
	res, err := f.p.VetoJob(ctx, f.tid, v.ID, jobs[0].ID, "user:u1")
	if err != nil || res.State != StateScheduled || res.Cancelled != 1 {
		t.Fatalf("first account: %+v %v", res, err)
	}
	if f.db.PublishJob.GetX(ctx, jobs[0].ID).Status != publishing.StatusCancelled || f.db.PublishJob.GetX(ctx, jobs[1].ID).Status != publishing.StatusPending {
		t.Fatal("only the chosen account's post is cancelled")
	}
	if st := readState(f.db.CreativeVersion.GetX(ctx, v.ID)); st.State != StateScheduled {
		t.Fatalf("the video still waits: %v", st.Raw)
	}
	if n := f.db.AuditLog.Query().Where(auditlog.TenantID(f.tid), auditlog.Event("autopilot.account_post_vetoed"), auditlog.Actor("user:u1")).CountX(ctx); n != 1 {
		t.Fatalf("audit rows: %d", n)
	}

	res, err = f.p.VetoJob(ctx, f.tid, v.ID, jobs[1].ID, "user:u2")
	if err != nil || res.State != StateVetoed {
		t.Fatalf("last account: %+v %v", res, err)
	}
	st := readState(f.db.CreativeVersion.GetX(ctx, v.ID))
	if st.State != StateVetoed || st.Raw["decision"].(map[string]any)["by"] != "user:u2" {
		t.Fatalf("vetoed by the last cancel: %v", st.Raw)
	}
}

// A post that reached the platform on one account answers too late for that account.
func TestVetoJobTooLate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, jobs := f.video(t, StateScheduled, f.now.Add(-time.Minute), publishing.StatusUploading, publishing.StatusPending)
	if _, err := f.p.VetoJob(ctx, f.tid, v.ID, jobs[0].ID, "user:u1"); !errors.Is(err, ErrTooLate) {
		t.Fatalf("too late: %v", err)
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
