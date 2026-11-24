package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
)

// ensureDefaultTemplate makes sure a platform template marked as the default follow-up exists.
func ensureDefaultTemplate(t *testing.T, db *ent.Client) {
	t.Helper()
	ctx := context.Background()
	if tpl, err := leadresponse.DefaultTemplate(ctx, db); err != nil {
		t.Fatal(err)
	} else if tpl != nil {
		return
	}
	home := seedTenant(t, db, "lr-home")
	t.Cleanup(func() { _, _ = db.NurtureSequence.Delete().Where(nurturesequence.TenantID(home)).Exec(ctx) })
	if _, err := db.NurtureSequence.Create().SetTenantID(home).SetScope("platform").SetName("Instant follow-up").
		SetTriggerEvent("lead.created").SetActive(true).
		SetSteps([]map[string]any{{"delay_hours": 0, "channel": "email", "optional": true, "subject": "Thanks", "body": "Hi {{first_name}}"}}).
		SetMetadata(map[string]any{leadresponse.TemplateFlag: true}).Save(ctx); err != nil {
		t.Fatal(err)
	}
}

func cleanupNurture(t *testing.T, db *ent.Client, tid uuid.UUID) {
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = db.ScheduledMeeting.Delete().Where(scheduledmeeting.TenantID(tid)).Exec(ctx)
		_, _ = db.NurtureEnrollment.Delete().Where(nurtureenrollment.TenantID(tid)).Exec(ctx)
		_, _ = db.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
	})
}

// Every tenant gets the follow-up by default, for leads created after the start only, and a
// tenant that switched it off gets nothing (owner, 2026-11-24).
func TestDefaultFollowUpEnrolsOnlyNewLeads(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	ensureDefaultTemplate(t, db)
	started, err := leadresponse.EnsureStartedAt(ctx, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tid := seedTenant(t, db, "lr-enrol")
	cleanupNurture(t, db, tid)
	mk := func(created time.Time) *ent.Lead {
		l, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetEmail("a@example.com").
			SetCreatedAt(created).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	old := mk(started.Add(-time.Hour))
	fresh := mk(time.Now().Add(time.Second))
	for _, l := range []*ent.Lead{old, fresh} {
		if err := enrolLead(ctx, db, zap.NewNop(), nil, leadCreated{TenantID: tid, LeadID: l.ID, LeadType: "customer", Source: "website"}, false); err != nil {
			t.Fatal(err)
		}
	}
	enrolled := func(id uuid.UUID) bool {
		ok, _ := db.NurtureEnrollment.Query().Where(nurtureenrollment.LeadID(id)).Exist(ctx)
		return ok
	}
	if enrolled(old.ID) {
		t.Fatal("a lead from before the start must never be followed up")
	}
	if !enrolled(fresh.ID) {
		t.Fatal("a new lead must get the default follow-up")
	}
	seqs, _ := db.NurtureSequence.Query().Where(nurturesequence.TenantID(tid)).All(ctx)
	if len(seqs) != 1 || !leadresponse.IsDefaultCopy(seqs[0]) {
		t.Fatalf("want the tenant's own copy, got %d sequences", len(seqs))
	}

	// Switched off: the copy stays, but new leads are not enrolled.
	if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{leadresponse.TenantKey: map[string]any{"follow_up": false}}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	later := mk(time.Now().Add(2 * time.Second))
	_ = enrolLead(ctx, db, zap.NewNop(), nil, leadCreated{TenantID: tid, LeadID: later.ID, LeadType: "customer"}, false)
	if enrolled(later.ID) {
		t.Fatal("follow-up switched off: no enrolment")
	}
}

// stepEnvelope is the nurture.step.due message for an enrollment.
func stepEnvelope(e *ent.NurtureEnrollment) fakeMsg {
	b, _ := json.Marshal(map[string]any{"tenant_id": e.TenantID.String(), "sequence_id": e.SequenceID.String(),
		"enrollment_id": e.ID.String(), "step_index": e.CurrentStep})
	return fakeMsg{data: b}
}

// A follow-up stops, with its reason, once the lead replied, booked or was contacted; an optional
// step the lead cannot receive is skipped without stopping the rest.
func TestFollowUpStopRulesAndOptionalSteps(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "lr-stop")
	cleanupNurture(t, db, tid)
	notif, sent := captureNotifications(t)
	h := &NurtureStepHandler{db: db, notif: notif, logger: zap.NewNop()}
	seq, err := db.NurtureSequence.Create().SetTenantID(tid).SetName("Follow-up").SetTriggerEvent("lead.created").SetActive(true).
		SetSteps([]map[string]any{
			{"delay_hours": 0, "channel": "whatsapp", "optional": true, "wa_template": "mf_lead_hello_v1_btn", "body": "Hi"},
			{"delay_hours": 0, "channel": "email", "optional": true, "subject": "Hi {{first_name}}", "body": "Thanks from {{fbo_name}}"},
			{"delay_hours": 24, "channel": "email", "subject": "Again", "body": "Following up"},
		}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	enrol := func(status string) (*ent.Lead, *ent.NurtureEnrollment) {
		l, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetStatus(status).
			SetEmail("lead@example.com").Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e, err := db.NurtureEnrollment.Create().SetTenantID(tid).SetSequenceID(seq.ID).SetLeadID(l.ID).
			SetCurrentStep(0).SetStatus("active").Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return l, e
	}
	run := func(e *ent.NurtureEnrollment) *ent.NurtureEnrollment {
		t.Helper()
		cur, _ := db.NurtureEnrollment.Get(ctx, e.ID)
		if err := h.Handle(ctx, stepEnvelope(cur)); err != nil {
			t.Fatal(err)
		}
		got, _ := db.NurtureEnrollment.Get(ctx, e.ID)
		return got
	}

	// No WhatsApp consent: the optional WhatsApp step is skipped, not turned into an email.
	_, e := enrol("new")
	if got := run(e); got.CurrentStep != 1 || got.Status != "active" || len(sent()) != 0 {
		t.Fatalf("WhatsApp step: step %d status %s sent %d", got.CurrentStep, got.Status, len(sent()))
	}
	if got := run(e); got.CurrentStep != 2 || len(sent()) != 1 {
		t.Fatalf("email step: step %d sent %d", got.CurrentStep, len(sent()))
	}
	if body := dataBody(sent()[0]); strings.Contains(body, "{{") {
		t.Fatalf("unrendered variable: %q", body)
	}

	// Contacted: stops before sending.
	l2, e2 := enrol("new")
	if _, err := leadresponse.MarkContacted(ctx, db, tid, l2.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := run(e2); got.Status != "cancelled" || deref(got.ExitReason) != "lead_contacted" {
		t.Fatalf("contacted: %s %s", got.Status, deref(got.ExitReason))
	}

	// The lead wrote back on WhatsApp after the follow-up started.
	l3, e3 := enrol("new")
	if _, err := db.Activity.Create().SetTenantID(tid).SetEntityType("lead").SetEntityID(l3.ID).SetActivityType("whatsapp").
		SetDirection("inbound").SetPerformedAt(time.Now().Add(time.Second)).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if got := run(e3); got.Status != "cancelled" || deref(got.ExitReason) != ExitLeadReplied {
		t.Fatalf("replied: %s %s", got.Status, deref(got.ExitReason))
	}

	// A meeting was booked.
	l4, e4 := enrol("new")
	if _, err := db.ScheduledMeeting.Create().SetTenantID(tid).SetLeadID(l4.ID).SetScheduledAt(time.Now().Add(24 * time.Hour)).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if got := run(e4); deref(got.ExitReason) != ExitMeetingBooked {
		t.Fatalf("booked: %s", deref(got.ExitReason))
	}
	if n := len(sent()); n != 1 {
		t.Fatalf("stopped follow-ups must send nothing, sent %d", n)
	}
}

// stepWhatsAppTemplate fills the template parameters and the booking button from the variables.
func TestStepWhatsAppTemplate(t *testing.T) {
	step := map[string]any{"wa_template": "mf_lead_hello_v1_btn", "wa_params": []any{"first_name", "fbo_name"}, "wa_button": "book/{{tenant_slug}}"}
	tpl := stepWhatsAppTemplate(step, map[string]any{"first_name": "Esther", "fbo_name": "Acme", "tenant_slug": "acme"})
	if tpl == nil || tpl.Name != "mf_lead_hello_v1_btn" || len(tpl.Params) != 2 || tpl.Params[1] != "Acme" || tpl.ButtonParam != "book/acme" {
		t.Fatalf("template %+v", tpl)
	}
	if stepWhatsAppTemplate(map[string]any{"body": "x"}, nil) != nil {
		t.Fatal("no template named: free text")
	}
}

// Reminders go out at each business-time step once, batched per tenant, and stop once the lead
// is contacted.
func TestLeadReminderStepsOnce(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	started, err := leadresponse.EnsureStartedAt(ctx, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tid := seedTenant(t, db, "lr-sla")
	seedTenantAdmin(t, db, tid, "owner@example.com")
	// Open around the clock so business time equals calendar time in this test.
	if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{leadresponse.TenantKey: map[string]any{
		"business_days": []any{0, 1, 2, 3, 4, 5, 6}, "business_hours": map[string]any{"open": "00:00", "close": "24:00"}}}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC()
	if created.Before(started) {
		created = started.Add(time.Second)
	}
	l, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetFirstName("Esther").
		SetInterest("Enterprise Software Development").SetCreatedAt(created).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	notif, sent := captureNotifications(t)
	j := NewLeadResponse(db, notif, nil, "https://portal", zap.NewNop())
	tn, _ := db.Tenant.Get(ctx, tid)
	platform := leadresponse.Fallback()
	at := func(d time.Duration) int {
		t.Helper()
		j.nowFn = func() time.Time { return created.Add(d) }
		n, err := j.remind(ctx, tn, platform, started)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if at(10*time.Minute) != 0 {
		t.Fatal("nothing before 15 minutes")
	}
	if at(3*time.Hour) != 1 { // past 15m and 2h: one email, at the 2 hour step
		t.Fatal("reminder due")
	}
	if body := dataBody(sent()[0]); !strings.Contains(body, "Esther") || !strings.Contains(body, "2 hours") {
		t.Fatalf("reminder body:\n%s", body)
	}
	if at(4*time.Hour) != 0 {
		t.Fatal("the same step must not repeat")
	}
	if at(25*time.Hour) != 1 || !strings.Contains(dataBody(sent()[1]), "last reminder") {
		t.Fatal("the 24 hour step is the last one")
	}
	if at(30*time.Hour) != 0 || len(sent()) != 2 {
		t.Fatal("no more reminders after the last step")
	}
	if got, _ := db.Lead.Get(ctx, l.ID); remindedStep(got) != 3 {
		t.Fatalf("reminded step %d, want 3", remindedStep(got))
	}

	// A contacted lead is never reminded about.
	l2, _ := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetCreatedAt(created).Save(ctx)
	_, _ = leadresponse.MarkContacted(ctx, db, tid, l2.ID, created.Add(time.Minute))
	if at(3*time.Hour) != 0 {
		t.Fatal("a contacted lead must not be reminded about")
	}
}

// With the morning brief switched off, the daily list of uncontacted leads still goes out on its
// own; with the list switched off too, nothing does.
func TestDailyListWithoutBrief(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	started, err := leadresponse.EnsureStartedAt(ctx, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tid := seedTenant(t, db, "lr-daily")
	seedTenantAdmin(t, db, tid, "owner@example.com")
	if _, err := db.Tenant.UpdateOneID(tid).SetTimezone("Africa/Nairobi").SetMetadata(map[string]any{DailyBriefOptOutKey: false}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC()
	if created.Before(started) {
		created = started.Add(time.Second)
	}
	if _, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetFirstName("Esther").
		SetCreatedAt(created).Save(ctx); err != nil {
		t.Fatal(err)
	}
	notif, sent := captureNotifications(t)
	job := NewDailyBrief(db, nil, notif, "", "", "https://portal", zap.NewNop())
	claimed := map[string]bool{}
	job.claim = func(_ context.Context, key string, _ time.Duration) bool {
		if claimed[key] {
			return false
		}
		claimed[key] = true
		return true
	}
	loc, _ := time.LoadLocation("Africa/Nairobi")
	next := created.In(loc).AddDate(0, 0, 1)
	job.nowFn = func() time.Time { return time.Date(next.Year(), next.Month(), next.Day(), 7, 0, 0, 0, loc) }
	if ok, err := job.forTenant(ctx, tid); err != nil || !ok {
		t.Fatalf("daily list not sent: %v %v", ok, err)
	}
	msgs := sent()
	if len(msgs) != 1 || !strings.Contains(dataBody(msgs[0]), "Esther, waiting") {
		t.Fatalf("daily list: %+v", msgs)
	}
	if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{DailyBriefOptOutKey: false,
		leadresponse.TenantKey: map[string]any{"daily_list": false}}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	job.nowFn = func() time.Time { return time.Date(next.Year(), next.Month(), next.Day()+1, 7, 0, 0, 0, loc) }
	if ok, _ := job.forTenant(ctx, tid); ok || len(sent()) != 1 {
		t.Fatal("brief and list both off: nothing is sent")
	}
}

// The daily list names the leads nobody contacted, with how long they waited.
func TestRenderUncontacted(t *testing.T) {
	now := time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC)
	name := "Esther"
	interest := "ERP"
	out := renderUncontacted(uncontactedList{Total: 3, Leads: []*ent.Lead{
		{FirstName: &name, Interest: &interest, CreatedAt: now.Add(-50 * time.Hour)},
		{CreatedAt: now.Add(-90 * time.Minute)},
	}}, now, "https://p/acme/leads?status=new")
	for _, want := range []string{"3 leads are still waiting", "- Esther, waiting 2 days (ERP)", "- A new lead, waiting 1 hour", "- and 1 more", "https://p/acme/leads?status=new"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}
