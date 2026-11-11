package contacts

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/account"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/auditlog"
	"github.com/bengobox/marketflow-api/internal/ent/chatsession"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/customfielddef"
	"github.com/bengobox/marketflow-api/internal/ent/customfieldvalue"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurturedeliveryevent"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
	"github.com/bengobox/marketflow-api/internal/ent/pipeline"
	"github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/ent/task"
)

// personFixture is a contact converted from a lead, with one of everything marketflow keeps about
// a person: activities and tasks on the contact and the lead, a custom field value, a chat, a
// meeting and a running nurture enrollment with a delivery event. A second person in the same
// tenant (other) must never be touched.
type personFixture struct {
	contactID, leadID, dealID, companyDealID, otherContactID uuid.UUID
}

func seedPerson(t *testing.T, svc *Service, client *ent.Client, tid uuid.UUID) personFixture {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = client.NurtureDeliveryEvent.Delete().Where(nurturedeliveryevent.TenantID(tid)).Exec(ctx)
		_, _ = client.NurtureEnrollment.Delete().Where(nurtureenrollment.TenantID(tid)).Exec(ctx)
		_, _ = client.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
		_, _ = client.CustomFieldValue.Delete().Where(customfieldvalue.TenantID(tid)).Exec(ctx)
		_, _ = client.CustomFieldDef.Delete().Where(customfielddef.TenantID(tid)).Exec(ctx)
		_, _ = client.Activity.Delete().Where(activity.TenantID(tid)).Exec(ctx)
		_, _ = client.Task.Delete().Where(task.TenantID(tid)).Exec(ctx)
		_, _ = client.ChatSession.Delete().Where(chatsession.TenantID(tid)).Exec(ctx)
		_, _ = client.ScheduledMeeting.Delete().Where(scheduledmeeting.TenantID(tid)).Exec(ctx)
		_, _ = client.AuditLog.Delete().Where(auditlog.TenantID(tid)).Exec(ctx)
		_, _ = client.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = client.Deal.Delete().Where(deal.TenantID(tid)).Exec(ctx)
		_, _ = client.PipelineStage.Delete().Where(pipelinestage.TenantID(tid)).Exec(ctx)
		_, _ = client.Pipeline.Delete().Where(pipeline.TenantID(tid)).Exec(ctx)
		_, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
		_, _ = client.Account.Delete().Where(account.TenantID(tid)).Exec(ctx)
		_, _ = client.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
	})
	l := client.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").
		SetEmail("jane@example.com").SetPhone("+254712345678").SetFirstName("Jane").SetLastName("Doe").
		SetInterest("Wants the aloe gel").SetMetadata(map[string]any{"note": "met at the fair"}).SaveX(ctx)
	c, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("0712345678"), Email: strPtr("jane@example.com"),
		FirstName: strPtr("Jane"), LastName: strPtr("Doe"), ContactType: "customer", LeadID: &l.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("0799000111"), FirstName: strPtr("Other"), ContactType: "customer"})
	if err != nil {
		t.Fatal(err)
	}
	pl := client.Pipeline.Create().SetTenantID(tid).SetName("Sales").SaveX(ctx)
	st := client.PipelineStage.Create().SetTenantID(tid).SetPipelineID(pl.ID).SetName("New").SaveX(ctx)
	d := client.Deal.Create().SetTenantID(tid).SetTitle("Jane Doe - 10 cases").SetPipelineID(pl.ID).SetStageID(st.ID).
		SetContactID(c.ID).SetValue(5000).SetMetadata(map[string]any{"note": "Jane's wedding order"}).SaveX(ctx)
	// A company (B2B) deal the person took part in: it holds other people's history too.
	acct := client.Account.Create().SetTenantID(tid).SetName("Acme Ltd").SaveX(ctx)
	cd := client.Deal.Create().SetTenantID(tid).SetTitle("Acme annual supply").SetPipelineID(pl.ID).SetStageID(st.ID).
		SetAccountID(acct.ID).SetContactID(c.ID).SetLeadID(l.ID).SetMetadata(map[string]any{"po": "PO-77"}).SaveX(ctx)
	for _, e := range []struct {
		typ string
		id  uuid.UUID
	}{{"contact", c.ID}, {"lead", l.ID}, {"deal", d.ID}, {"deal", cd.ID}, {"contact", other.ID}} {
		client.Activity.Create().SetTenantID(tid).SetEntityType(e.typ).SetEntityID(e.id).SetActivityType("note").
			SetSubject("Call with Jane").SetBody("Jane said her daughter is ill").SetMetadata(map[string]any{"from": "jane@example.com"}).ExecX(ctx)
		client.Task.Create().SetTenantID(tid).SetEntityType(e.typ).SetEntityID(e.id).SetTitle("Call Jane Doe").
			SetDescription("Ask about the order").ExecX(ctx)
	}
	def := client.CustomFieldDef.Create().SetTenantID(tid).SetEntityType("contact").SetFieldKey("birthday").SetFieldLabel("Birthday").SaveX(ctx)
	client.CustomFieldValue.Create().SetTenantID(tid).SetEntityType("contact").SetEntityID(c.ID).SetFieldDefID(def.ID).SetValue("1990-01-01").ExecX(ctx)
	client.ChatSession.Create().SetTenantID(tid).SetContactID(c.ID).
		SetMessages([]map[string]any{{"role": "user", "content": "I am Jane, call me on 0712345678"}}).SetMessageCount(1).ExecX(ctx)
	client.ScheduledMeeting.Create().SetTenantID(tid).SetLeadID(l.ID).SetScheduledAt(time.Now().Add(24 * time.Hour)).
		SetAttendeeEmail("jane@example.com").SetAttendeeName("Jane Doe").SetNotes("Bring samples").ExecX(ctx)
	seq := client.NurtureSequence.Create().SetTenantID(tid).SetName("Welcome").SetTriggerEvent("lead.created").
		SetActive(true).SetSteps([]map[string]any{}).SaveX(ctx)
	enr := client.NurtureEnrollment.Create().SetTenantID(tid).SetSequenceID(seq.ID).SetContactID(c.ID).
		SetNextStepAt(time.Now().Add(time.Hour)).SetMetadata(map[string]any{"recipient": "jane@example.com"}).SaveX(ctx)
	client.NurtureDeliveryEvent.Create().SetTenantID(tid).SetEnrollmentID(enr.ID).SetChannel("email").SetEventType("sent").
		SetMetadata(map[string]any{"to": "jane@example.com"}).ExecX(ctx)
	return personFixture{contactID: c.ID, leadID: l.ID, dealID: d.ID, companyDealID: cd.ID, otherContactID: other.ID}
}

// assertCompanyDealUnlinkedOnly checks the company (B2B) deal kept its title, metadata, account,
// activity and task untouched, and lost only the erased person's contact and lead link.
func assertCompanyDealUnlinkedOnly(t *testing.T, client *ent.Client, p personFixture) {
	t.Helper()
	ctx := context.Background()
	d := client.Deal.GetX(ctx, p.companyDealID)
	if d.Title != "Acme annual supply" || d.Metadata["po"] != "PO-77" || d.AccountID == nil {
		t.Fatalf("company deal was changed: %+v", d)
	}
	if d.ContactID != nil || d.LeadID != nil {
		t.Fatalf("company deal still links the person: contact=%v lead=%v", d.ContactID, d.LeadID)
	}
	a := client.Activity.Query().Where(activity.EntityType("deal"), activity.EntityID(p.companyDealID)).OnlyX(ctx)
	if a.Subject == nil || a.Body == nil {
		t.Fatal("the company deal's activity was scrubbed")
	}
	tk := client.Task.Query().Where(task.EntityType("deal"), task.EntityID(p.companyDealID)).OnlyX(ctx)
	if tk.Title == ErasedName || tk.Description == nil {
		t.Fatal("the company deal's task was scrubbed")
	}
}

// The strip scrubs everything marketflow holds about the person, keeping ids and counts, and
// leaves another person's records alone.
func TestEraseScrubsPersonRecords(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	p := seedPerson(t, svc, client, tid)

	if changed, err := svc.Erase(ctx, tid, p.contactID, ErasureRecord{Source: "privacy.party_anonymised", Reasons: []string{"pos: referenced"}}); err != nil || !changed {
		t.Fatalf("erase: %v %v", changed, err)
	}

	l := client.Lead.GetX(ctx, p.leadID)
	if l.Email != nil || l.Phone != nil || l.LastName != nil || l.Interest != nil || l.FirstName == nil || *l.FirstName != ErasedName {
		t.Fatalf("lead not scrubbed: %+v", l)
	}
	if _, ok := l.Metadata["note"]; ok {
		t.Fatal("lead metadata kept")
	}
	for _, a := range client.Activity.Query().Where(activity.TenantID(tid)).AllX(ctx) {
		personal := a.Subject != nil || a.Body != nil || len(a.Metadata) > 0
		if mine := a.EntityID == p.contactID || a.EntityID == p.leadID || a.EntityID == p.dealID; mine == personal {
			t.Fatalf("activity on %s: scrubbed=%v", a.EntityID, !personal)
		}
	}
	for _, tk := range client.Task.Query().Where(task.TenantID(tid)).AllX(ctx) {
		mine := tk.EntityID == p.contactID || tk.EntityID == p.leadID || tk.EntityID == p.dealID
		if mine != (tk.Title == ErasedName && tk.Description == nil) {
			t.Fatalf("task on %s: %q", tk.EntityID, tk.Title)
		}
	}
	if d := client.Deal.GetX(ctx, p.dealID); d.Title != ErasedName || len(d.Metadata) > 0 || d.Value == nil || *d.Value != 5000 {
		t.Fatalf("deal: %+v", d)
	}
	// The company deal keeps its title, metadata, activities and tasks (checked in the loops
	// above: they are not the person's) and only loses the person's link.
	assertCompanyDealUnlinkedOnly(t, client, p)
	if n := client.CustomFieldValue.Query().Where(customfieldvalue.TenantID(tid)).CountX(ctx); n != 0 {
		t.Fatalf("custom field values left: %d", n)
	}
	chat := client.ChatSession.Query().Where(chatsession.TenantID(tid)).OnlyX(ctx)
	if len(chat.Messages) != 0 || chat.MessageCount != 1 || chat.ContactID == nil {
		t.Fatalf("chat: %+v", chat)
	}
	m := client.ScheduledMeeting.Query().Where(scheduledmeeting.TenantID(tid)).OnlyX(ctx)
	if m.AttendeeEmail != nil || m.AttendeeName != nil || m.Notes != nil {
		t.Fatalf("meeting: %+v", m)
	}
	e := client.NurtureEnrollment.Query().Where(nurtureenrollment.TenantID(tid)).OnlyX(ctx)
	if e.Status != "cancelled" || e.ExitReason == nil || *e.ExitReason != "erased" || e.NextStepAt != nil || len(e.Metadata) > 0 {
		t.Fatalf("enrollment: %+v", e)
	}
	if ev := client.NurtureDeliveryEvent.Query().Where(nurturedeliveryevent.TenantID(tid)).OnlyX(ctx); len(ev.Metadata) > 0 {
		t.Fatalf("delivery event metadata kept: %v", ev.Metadata)
	}
	if o := client.Contact.GetX(ctx, p.otherContactID); o.FirstName == nil || *o.FirstName != "Other" {
		t.Fatal("another person's contact was touched")
	}
}

// With no other service linking the contact, DeleteErased removes the contact and the person's
// own records, scrubs the lead (kept for campaign counts), publishes crm.contact.deleted once and
// keeps an audit row; a second call is a no-op.
func TestDeleteErasedRemovesPersonRecords(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	p := seedPerson(t, svc, client, tid)
	rec := ErasureRecord{Source: "privacy.party_anonymised", SourceID: uuid.NewString()}

	if deleted, err := svc.DeleteErased(ctx, tid, p.contactID, rec); err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	if ok, _ := svc.Exists(ctx, tid, p.contactID); ok {
		t.Fatal("contact still there")
	}
	mine := func(n int, what string) {
		if n != 0 {
			t.Fatalf("%s left: %d", what, n)
		}
	}
	mine(client.Activity.Query().Where(activity.TenantID(tid), activity.EntityIDIn(p.contactID, p.leadID, p.dealID)).CountX(ctx), "activities")
	mine(client.Task.Query().Where(task.TenantID(tid), task.EntityIDIn(p.contactID, p.leadID, p.dealID)).CountX(ctx), "tasks")
	// The deal stays for pipeline reports, unlinked from the deleted contact and scrubbed.
	if d := client.Deal.GetX(ctx, p.dealID); d.Title != ErasedName || len(d.Metadata) > 0 || d.ContactID != nil {
		t.Fatalf("deal: %+v", d)
	}
	mine(client.CustomFieldValue.Query().Where(customfieldvalue.TenantID(tid)).CountX(ctx), "custom field values")
	mine(client.ChatSession.Query().Where(chatsession.TenantID(tid)).CountX(ctx), "chat sessions")
	mine(client.ScheduledMeeting.Query().Where(scheduledmeeting.TenantID(tid)).CountX(ctx), "meetings")
	mine(client.NurtureEnrollment.Query().Where(nurtureenrollment.TenantID(tid)).CountX(ctx), "enrollments")
	mine(client.NurtureDeliveryEvent.Query().Where(nurturedeliveryevent.TenantID(tid)).CountX(ctx), "delivery events")
	if n := client.Activity.Query().Where(activity.TenantID(tid), activity.EntityID(p.otherContactID)).CountX(ctx); n != 1 {
		t.Fatalf("another person's activity was removed: %d", n)
	}
	assertCompanyDealUnlinkedOnly(t, client, p)
	if l := client.Lead.GetX(ctx, p.leadID); l.Email != nil || l.Phone != nil || l.LastName != nil {
		t.Fatalf("lead not scrubbed: %+v", l)
	}
	if n := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid), outboxevent.EventType("contact.deleted")).CountX(ctx); n != 1 {
		t.Fatalf("contact.deleted rows = %d", n)
	}
	if n := client.AuditLog.Query().Where(auditlog.TenantID(tid), auditlog.Event(ContactErasedDeletedAudit)).CountX(ctx); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	if deleted, err := svc.DeleteErased(ctx, tid, p.contactID, rec); err != nil || deleted {
		t.Fatalf("second delete: %v %v", deleted, err)
	}
}

// A given-up erasure leaves an audit row naming the contact, with no personal data.
func TestRecordErasureFailure(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() { _, _ = client.AuditLog.Delete().Where(auditlog.TenantID(tid)).Exec(context.Background()) })
	cid := uuid.New()
	if err := svc.RecordErasureFailure(ctx, tid, cid, ErasureRecord{Source: "privacy.party_anonymised", SourceID: "p1"}, "gave up"); err != nil {
		t.Fatal(err)
	}
	row := client.AuditLog.Query().Where(auditlog.TenantID(tid), auditlog.Event(ContactErasureFailedAudit)).OnlyX(ctx)
	if row.SubjectID == nil || *row.SubjectID != cid || row.Payload["failure"] != "gave up" || row.Payload["source_id"] != "p1" {
		t.Fatalf("row = %+v", row)
	}
}

// Erase strips a contact to a minimal record: no email, phone or other personal fields, the
// name "Erased", the same id. It records why in the metadata and in a tenant audit row, tells
// other services through crm.contact.updated, and a second call changes and records nothing.
func TestEraseStripsContactOnce(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() {
		_, _ = client.AuditLog.Delete().Where(auditlog.TenantID(tid)).Exec(ctx)
		_, _ = client.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
	})

	c, err := svc.Create(ctx, CreateInput{
		TenantID: tid, Phone: strPtr("0712345678"), Email: strPtr("jane@example.com"),
		FirstName: strPtr("Jane"), LastName: strPtr("Doe"), ContactType: "customer",
		Metadata: map[string]any{"legacy_contact_id": "C-0042", "address": "Plot 7, Riverside"},
	})
	if err != nil {
		t.Fatal(err)
	}
	party := uuid.New()
	rec := ErasureRecord{Source: "privacy.party_anonymised", SourceID: party.String(), Reasons: []string{"treasury: referenced"}}

	changed, err := svc.Erase(ctx, tid, c.ID, rec)
	if err != nil || !changed {
		t.Fatalf("erase: changed=%v err=%v", changed, err)
	}
	got, err := client.Contact.Get(ctx, c.ID)
	if err != nil {
		t.Fatalf("the contact must be kept: %v", err)
	}
	if got.Email != nil || got.Phone != nil || got.EmailNormalized != nil || got.PhoneSubscriberDigits != nil || got.LastName != nil {
		t.Fatalf("personal fields left: %+v", got)
	}
	if got.FirstName == nil || *got.FirstName != "Erased" {
		t.Fatalf("name = %v", got.FirstName)
	}
	if got.SubscribedEmail || got.SubscribedSms {
		t.Fatal("an erased contact must not stay subscribed")
	}
	if _, ok := got.Metadata["address"]; ok {
		t.Fatal("free-form personal metadata must go")
	}
	if got.Metadata["legacy_contact_id"] != "C-0042" || got.Metadata["erased_at"] == nil ||
		got.Metadata["erasure_source"] != "privacy.party_anonymised" || got.Metadata["erasure_source_id"] != party.String() {
		t.Fatalf("metadata = %v", got.Metadata)
	}

	audits, err := client.AuditLog.Query().Where(auditlog.TenantID(tid), auditlog.Event("contact.erased")).All(ctx)
	if err != nil || len(audits) != 1 || audits[0].SubjectID == nil || *audits[0].SubjectID != c.ID {
		t.Fatalf("audit rows = %d err=%v", len(audits), err)
	}
	updated, err := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid), outboxevent.EventType("contact.updated")).All(ctx)
	if err != nil || len(updated) != 1 {
		t.Fatalf("contact.updated rows = %d err=%v", len(updated), err)
	}
	var ev struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(updated[0].Payload, &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Payload) != 2 || ev.Payload["contact_id"] != c.ID.String() {
		t.Fatalf("the event must carry ids and field names only: %v", ev.Payload)
	}

	// Redelivery: nothing changes, nothing more is recorded or published.
	changed, err = svc.Erase(ctx, tid, c.ID, rec)
	if err != nil || changed {
		t.Fatalf("second erase: changed=%v err=%v", changed, err)
	}
	if n := client.AuditLog.Query().Where(auditlog.TenantID(tid), auditlog.Event("contact.erased")).CountX(ctx); n != 1 {
		t.Fatalf("audit rows after redelivery = %d", n)
	}

	// Another tenant cannot erase it.
	if changed, err := svc.Erase(ctx, uuid.New(), c.ID, rec); err != nil || changed {
		t.Fatalf("cross-tenant erase: changed=%v err=%v", changed, err)
	}
	if ok, err := svc.Exists(ctx, uuid.New(), c.ID); err != nil || ok {
		t.Fatalf("exists across tenants: %v %v", ok, err)
	}
	if ok, err := svc.Exists(ctx, tid, c.ID); err != nil || !ok {
		t.Fatalf("exists: %v %v", ok, err)
	}
}
