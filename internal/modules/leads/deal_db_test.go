package leads

import (
	"context"
	"sync"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	entcontact "github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/pipeline"
	"github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
)

func seedDealsTenant(t *testing.T, db *ent.Client) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := seedLeadsTenant(t, db)
	t.Cleanup(func() { // runs before the tenant's own cleanup
		_, _ = db.Deal.Delete().Where(deal.TenantID(tid)).Exec(ctx)
		_, _ = db.PipelineStage.Delete().Where(pipelinestage.TenantID(tid)).Exec(ctx)
		_, _ = db.Pipeline.Delete().Where(pipeline.TenantID(tid)).Exec(ctx)
		_, _ = db.Contact.Delete().Where(entcontact.TenantID(tid)).Exec(ctx)
		_, _ = db.ServiceConfig.Delete().Where(serviceconfig.TenantID(tid)).Exec(ctx)
	})
	return tid
}

func TestQualifiedLeadGetsOneDeal(t *testing.T) {
	svc, db := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedDealsTenant(t, db)
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{"currency": "UGX"}).ExecX(ctx)
	owner := uuid.New()
	l := db.Lead.Create().SetTenantID(tid).SetFirstName("Esther").SetLastName("Masika").SetPhone("0712345678").
		SetInterest("business_opportunity").SetOwnerUserID(owner).SaveX(ctx)
	c := db.Contact.Create().SetTenantID(tid).SetPhone("+254712345678").SetPhoneSubscriberDigits("712345678").SaveX(ctx)
	other := seedDealsTenant(t, db)
	db.Contact.Create().SetTenantID(other).SetPhone("+254712345678").SetPhoneSubscriberDigits("712345678").ExecX(ctx)

	set := func(status string) {
		t.Helper()
		if _, err := svc.Update(ctx, tid, l.ID, UpdateInput{Status: &status}); err != nil {
			t.Fatal(err)
		}
	}
	set("contacted")
	if n := db.Deal.Query().Where(deal.TenantID(tid)).CountX(ctx); n != 0 {
		t.Fatalf("a contacted lead has %d deals", n)
	}
	// Two people qualify it at once, then it converts, is reopened and qualified again.
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := "qualified"
			if _, err := svc.Update(ctx, tid, l.ID, UpdateInput{Status: &q}); err != nil {
				t.Error(err)
			}
			if _, err := svc.EnsureDeal(ctx, tid, l.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, s := range []string{"converted", "contacted", "qualified"} {
		set(s)
	}
	d := db.Deal.Query().Where(deal.TenantID(tid)).OnlyX(ctx)
	st := db.PipelineStage.GetX(ctx, d.StageID)
	if st.Name != "New" || d.Title != "Esther Masika, business opportunity" || d.Currency != "UGX" || d.Value != nil ||
		d.LeadID == nil || *d.LeadID != l.ID || d.ContactID == nil || *d.ContactID != c.ID || d.OwnerUserID == nil || *d.OwnerUserID != owner {
		t.Fatalf("deal %+v in %s", d, st.Name)
	}
	if db.Pipeline.Query().Where(pipeline.TenantID(tid)).CountX(ctx) != 1 || db.Deal.Query().Where(deal.TenantID(other)).CountX(ctx) != 0 {
		t.Fatal("one pipeline, and nothing in another tenant")
	}
	if got := db.Lead.GetX(ctx, l.ID).Metadata["deal_id"]; got != d.ID.String() {
		t.Fatalf("lead deal_id %v", got)
	}
	// A deal somebody deleted stays deleted.
	db.Deal.DeleteOneID(d.ID).ExecX(ctx)
	if made, err := svc.EnsureDeal(ctx, tid, l.ID); err != nil || made != nil {
		t.Fatalf("made again: %v %v", made, err)
	}
	// Another tenant cannot reach it.
	if _, err := svc.EnsureDeal(ctx, other, l.ID); err == nil {
		t.Fatal("another tenant's lead")
	}
}

func TestNoDealForNonCustomersOrOptOuts(t *testing.T) {
	svc, db := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedDealsTenant(t, db)
	q := "qualified"
	mk := func(email string, f func(*ent.LeadCreate)) *ent.Lead {
		b := db.Lead.Create().SetTenantID(tid).SetEmail(email)
		if f != nil {
			f(b)
		}
		return b.SaveX(ctx)
	}
	recruit := mk("recruit@example.com", func(b *ent.LeadCreate) { b.SetLeadType("recruit") })
	job := mk("job@example.com", func(b *ent.LeadCreate) {
		b.SetMetadata(map[string]any{"message": "Please find my CV, I am looking for a job"})
	})
	out := mk("Out@Example.com", nil)
	if err := contacts.MarkOptedOut(ctx, db, tid, "email", "out@example.com", "2026-11-25T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Opted out before opt-outs were recorded: a consent switched on by staff, off now.
	old := mk("old@example.com", nil)
	db.Contact.Create().SetTenantID(tid).SetEmail("old@example.com").SetEmailNormalized("old@example.com").
		SetMetadata(map[string]any{"consent_source": "recorded_by_staff"}).ExecX(ctx)
	// Never asked for marketing consent: still a customer.
	plain := mk("plain@example.com", nil)
	db.Contact.Create().SetTenantID(tid).SetEmail("plain@example.com").SetEmailNormalized("plain@example.com").ExecX(ctx)
	for _, l := range []*ent.Lead{recruit, job, out, old, plain} {
		if _, err := svc.Update(ctx, tid, l.ID, UpdateInput{Status: &q}); err != nil {
			t.Fatal(err)
		}
	}
	d := db.Deal.Query().Where(deal.TenantID(tid)).AllX(ctx)
	if len(d) != 1 || *d[0].LeadID != plain.ID {
		t.Fatalf("deals %+v", d)
	}
	db.ServiceConfig.Create().SetTenantID(tid).SetConfigKey(DealFromLeadKey).SetConfigValue("off").SetConfigType("string").ExecX(ctx)
	off := mk("off@example.com", nil)
	if _, err := svc.Update(ctx, tid, off.ID, UpdateInput{Status: &q}); err != nil || db.Deal.Query().Where(deal.TenantID(tid)).CountX(ctx) != 1 {
		t.Fatal("switched off for the tenant", err)
	}
}

// A deal that cannot be made never fails the lead, and the worker makes it later.
func TestMissedDealIsRepaired(t *testing.T) {
	svc, db := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedDealsTenant(t, db)
	p := db.Pipeline.Create().SetTenantID(tid).SetName("Sales").SetIsDefault(true).SaveX(ctx)
	l := db.Lead.Create().SetTenantID(tid).SetEmail("late@example.com").SaveX(ctx)
	conv := "converted"
	got, err := svc.Update(ctx, tid, l.ID, UpdateInput{Status: &conv})
	if err != nil || got.Status != "converted" {
		t.Fatalf("the lead is saved: %v %v", got, err)
	}
	if db.Deal.Query().Where(deal.TenantID(tid)).CountX(ctx) != 0 {
		t.Fatal("no stage, no deal")
	}
	db.PipelineStage.Create().SetTenantID(tid).SetPipelineID(p.ID).SetName("Won").SetStageOrder(1).SetIsWonStage(true).ExecX(ctx)
	db.PipelineStage.Create().SetTenantID(tid).SetPipelineID(p.ID).SetName("Second").SetStageOrder(3).ExecX(ctx)
	first := db.PipelineStage.Create().SetTenantID(tid).SetPipelineID(p.ID).SetName("First").SetStageOrder(2).SaveX(ctx)
	// Only this test's tenant.
	mine := func(s *entsql.Selector) { s.Where(entsql.EQ(s.C(lead.FieldTenantID), tid)) }
	if n, err := svc.repairDeals(ctx, time.Now().Add(time.Hour), mine); err != nil || n != 0 {
		t.Fatalf("too old: %d %v", n, err)
	}
	for _, want := range []int{1, 0} {
		if n, err := svc.repairDeals(ctx, time.Now().Add(-time.Hour), mine); err != nil || n != want {
			t.Fatalf("repaired %d %v, want %d", n, err, want)
		}
	}
	if d := db.Deal.Query().Where(deal.TenantID(tid)).OnlyX(ctx); d.StageID != first.ID {
		t.Fatal("first open stage")
	}
}
