package leads

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
)

func cleanActivities(t *testing.T, client *ent.Client, tid uuid.UUID) {
	t.Cleanup(func() {
		_, _ = client.Activity.Delete().Where(activity.TenantID(tid)).Exec(context.Background())
	})
}

func outboxTypes(t *testing.T, client *ent.Client, tid uuid.UUID) map[string]int {
	t.Helper()
	rows, err := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid)).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range rows {
		out[r.EventType]++
	}
	return out
}

func TestPhoneAndEmailKeys(t *testing.T) {
	for _, p := range []string{"0712345678", "+254 712 345 678", "254712345678", "(0712) 345-678"} {
		if got := PhoneKey(p); got != "712345678" {
			t.Fatalf("PhoneKey(%q) = %q", p, got)
		}
	}
	if PhoneKey("12345") != "" {
		t.Fatal("too short to be a number")
	}
	if EmailKey("  Jane@Example.COM ") != "jane@example.com" {
		t.Fatal("email key")
	}
}

func TestMergeMetadataKeepsOtherKeys(t *testing.T) {
	got := mergeMetadata(map[string]any{"whatsapp_consent": true, "gclid": "g1", "old": 1}, map[string]any{"notes": "call back", "old": nil})
	if got["whatsapp_consent"] != true || got["gclid"] != "g1" || got["notes"] != "call back" {
		t.Fatalf("merge lost keys: %v", got)
	}
	if _, ok := got["old"]; ok {
		t.Fatal("a null value removes the key")
	}
}

// One person is one lead: a second submission with the same email (other case) or the same
// phone in another format lands on the first lead as an activity with its own attribution, and
// only the first writes lead.created.
func TestCaptureDedupesByEmailAndPhone(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedLeadsTenant(t, client)
	cleanActivities(t, client, tid)
	f, err := client.Funnel.Create().SetTenantID(tid).SetName("B").SetSlug("b-" + tid.String()).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = client.Lead.Delete().Where(lead.TenantID(tid)).Exec(context.Background())
		_ = client.Funnel.DeleteOneID(f.ID).Exec(context.Background())
	})
	funnelB := f.ID

	first, err := svc.Capture(ctx, CreateInput{TenantID: tid, Source: "funnel", LeadType: "customer",
		Email: strp("Jane@Example.com"), FirstName: strp("Jane"), Metadata: map[string]any{"gclid": "first"}})
	if err != nil || !first.Created {
		t.Fatalf("first capture: %+v %v", first, err)
	}
	again, err := svc.Capture(ctx, CreateInput{TenantID: tid, Source: "website_contact", LeadType: "customer",
		Email: strp("jane@example.com "), Phone: strp("0712 345 678"), FunnelID: &funnelB, Interest: strp("POS for my shop"),
		Metadata: map[string]any{"gclid": "second", "whatsapp_consent": true}})
	if err != nil || again.Created || again.Lead.ID != first.Lead.ID {
		t.Fatalf("second capture should merge: %+v %v", again, err)
	}
	byPhone, err := svc.Capture(ctx, CreateInput{TenantID: tid, Source: "widget", LeadType: "customer", Phone: strp("+254712345678")})
	if err != nil || byPhone.Created || byPhone.Lead.ID != first.Lead.ID {
		t.Fatalf("phone match should merge: %+v %v", byPhone, err)
	}

	if n, _ := client.Lead.Query().Where(lead.TenantID(tid)).Count(ctx); n != 1 {
		t.Fatalf("leads = %d, want 1", n)
	}
	l, _ := client.Lead.Get(ctx, first.Lead.ID)
	if l.Source != "funnel" || l.Phone == nil || l.Interest == nil || l.FunnelID == nil || *l.FunnelID != funnelB {
		t.Fatalf("origin kept and empty fields filled: %+v", l)
	}
	if l.Metadata["gclid"] != "first" || l.Metadata["whatsapp_consent"] != true || l.Metadata["submission_count"] != float64(3) {
		t.Fatalf("metadata: %v", l.Metadata)
	}
	subs, err := svc.Submissions(ctx, tid, l.ID, 10)
	if err != nil || len(subs) != 2 {
		t.Fatalf("submissions = %d (%v), want 2", len(subs), err)
	}
	var sawSecond bool
	for _, s := range subs {
		if s.Metadata["gclid"] == "second" && s.Metadata["funnel_id"] == funnelB.String() && s.Metadata["source"] == "website_contact" {
			sawSecond = true
		}
	}
	if !sawSecond {
		t.Fatal("the repeat submission keeps its own funnel, source and click id")
	}
	if got := outboxTypes(t, client, tid); got[LeadCreatedEvent] != 1 || got[LeadResubmittedEvent] != 2 {
		t.Fatalf("events = %v, want 1 created and 2 resubmitted", got)
	}
}

func TestCaptureReopensLostLead(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedLeadsTenant(t, client)
	cleanActivities(t, client, tid)
	first, err := svc.Capture(ctx, CreateInput{TenantID: tid, Source: "funnel", LeadType: "customer", Email: strp("a@b.co")})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, tid, first.Lead.ID); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Capture(ctx, CreateInput{TenantID: tid, Source: "funnel", LeadType: "customer", Email: strp("a@b.co")})
	if err != nil || again.Lead.Status != "new" {
		t.Fatalf("a lost lead that comes back is new again: %+v %v", again.Lead, err)
	}
}

// Two submissions of the same new person at the same moment make one lead (advisory lock).
func TestCaptureConcurrentSamePersonOneLead(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedLeadsTenant(t, client)
	cleanActivities(t, client, tid)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Capture(ctx, CreateInput{TenantID: tid, Source: "funnel", LeadType: "customer", Phone: strp("0700111222")}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n, _ := client.Lead.Query().Where(lead.TenantID(tid)).Count(ctx); n != 1 {
		t.Fatalf("leads = %d, want 1", n)
	}
}

// Other tenants' leads never match.
func TestCaptureIsTenantScoped(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	a, b := seedLeadsTenant(t, client), seedLeadsTenant(t, client)
	for _, tid := range []uuid.UUID{a, b} {
		res, err := svc.Capture(ctx, CreateInput{TenantID: tid, Source: "funnel", LeadType: "customer", Email: strp("same@x.co")})
		if err != nil || !res.Created {
			t.Fatalf("tenant %s: %+v %v", tid, res, err)
		}
	}
}

// A migration keeps created time, status and source, writes no events, dedupes against existing
// leads, and a second run changes nothing.
func TestImportIsIdempotentAndQuiet(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedLeadsTenant(t, client)
	cleanActivities(t, client, tid)
	existing, err := svc.Capture(ctx, CreateInput{TenantID: tid, Source: "widget", LeadType: "customer", Email: strp("known@x.co")})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 8, 5, 14, 11, 48, 0, time.UTC)
	rows := []ImportRow{
		{ExternalRef: "site:contact:1", CreatedAt: old, Lead: CreateInput{Source: "website_contact", Status: "contacted", LeadType: "customer",
			Email: strp("new@x.co"), FirstName: strp("Amina"), Metadata: map[string]any{"message": "Need a website"}}},
		{ExternalRef: "site:lead:2", CreatedAt: old.Add(time.Hour), Lead: CreateInput{Source: "manual", LeadType: "customer", Email: strp("KNOWN@x.co")}},
		{ExternalRef: "site:lead:3", CreatedAt: old.Add(2 * time.Hour), Lead: CreateInput{Source: "manual", LeadType: "customer", Phone: strp("0799000111")}},
	}
	before := outboxTypes(t, client, tid)
	res, err := svc.Import(ctx, tid, rows)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ImportCreated, ImportMerged, ImportCreated}
	for i, r := range res {
		if r.Outcome != want[i] {
			t.Fatalf("row %d outcome %s, want %s", i, r.Outcome, want[i])
		}
	}
	if res[1].LeadID != existing.Lead.ID {
		t.Fatal("the merged row reports the existing lead")
	}
	l, err := client.Lead.Get(ctx, ImportLeadID(tid, "site:contact:1"))
	if err != nil || !l.CreatedAt.Equal(old) || l.Status != "contacted" || l.Source != "website_contact" || l.Metadata["imported_from"] != "site:contact:1" {
		t.Fatalf("imported lead: %+v %v", l, err)
	}
	again, err := svc.Import(ctx, tid, rows)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range again {
		if r.Outcome != ImportSkipped || r.LeadID != res[i].LeadID {
			t.Fatalf("re-run row %d: %+v", i, r)
		}
	}
	if n, _ := client.Lead.Query().Where(lead.TenantID(tid)).Count(ctx); n != 3 {
		t.Fatalf("leads = %d, want 3", n)
	}
	after := outboxTypes(t, client, tid)
	if after[LeadCreatedEvent] != before[LeadCreatedEvent] || after[LeadResubmittedEvent] != 0 {
		t.Fatalf("import wrote events: before %v after %v", before, after)
	}
}

func TestStatsAndSourceFilters(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedLeadsTenant(t, client)
	cleanActivities(t, client, tid)
	for i, src := range []string{"website_contact", "website_attachment", "manual", "manual"} {
		if _, err := svc.Capture(ctx, CreateInput{TenantID: tid, Source: src, LeadType: "customer", Phone: strp(fmt.Sprintf("0711%06d", (i+1)*1111))}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := svc.Stats(ctx, ListInput{TenantID: tid})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 4 || st.ByStatus["new"] != 4 || st.BySource["manual"] != 2 || st.ThisWeek != 4 {
		t.Fatalf("stats: %+v", st)
	}
	enq, err := svc.Stats(ctx, ListInput{TenantID: tid, Sources: []string{"website_contact", "website_attachment"}})
	if err != nil || enq.Total != 2 {
		t.Fatalf("enquiry stats: %+v %v", enq, err)
	}
	_, n, err := svc.List(ctx, ListInput{TenantID: tid, ExcludeSources: []string{"website_contact", "website_attachment"}})
	if err != nil || n != 2 {
		t.Fatalf("excluded list = %d %v", n, err)
	}
}
