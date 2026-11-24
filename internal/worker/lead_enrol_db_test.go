package worker

import (
	"context"
	"database/sql"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
)

func resubmittedMsg(t *testing.T, tid, lid uuid.UUID, funnelID *uuid.UUID) fakeMsg {
	t.Helper()
	p := map[string]any{"lead_id": lid.String(), "lead_type": "customer", "source": "funnel", "activity_id": uuid.NewString()}
	if funnelID != nil {
		p["funnel_id"] = funnelID.String()
	}
	raw, err := eventslib.NewEvent(leads.LeadResubmittedEvent, leads.LeadAggregate, lid, tid, p).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return fakeMsg{data: raw}
}

// Enrolment is keyed by lead and funnel: a person who comes back through a new funnel is enrolled
// in that funnel's sequence, never twice for the same funnel, and a repeat through the original
// funnel enrols nothing.
func TestResubmissionEnrolsOncePerFunnel(t *testing.T) {
	sqlDB, err := sql.Open("pgx", testWorkerPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	defer sqlDB.Close()
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer client.Close()
	ctx := context.Background()

	tid := uuid.New()
	if _, err := client.Tenant.Create().SetID(tid).SetName("Enrol Test").SetSlug("enrol-test-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = client.NurtureEnrollment.Delete().Where(nurtureenrollment.TenantID(tid)).Exec(ctx)
		_, _ = client.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
		_, _ = client.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_, _ = client.Funnel.Delete().Where(funnel.TenantID(tid)).Exec(ctx)
		_ = client.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	fa, _ := client.Funnel.Create().SetTenantID(tid).SetName("A").SetSlug("a-" + tid.String()).Save(ctx)
	fb, _ := client.Funnel.Create().SetTenantID(tid).SetName("B").SetSlug("b-" + tid.String()).Save(ctx)
	generic, err := client.NurtureSequence.Create().SetTenantID(tid).SetName("Welcome").SetTriggerEvent("lead.created").
		SetActive(true).SetSteps([]map[string]any{}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forB, err := client.NurtureSequence.Create().SetTenantID(tid).SetName("Funnel B").SetTriggerEvent("lead.created").
		SetActive(true).SetSteps([]map[string]any{}).SetMetadata(map[string]any{"funnel_id": fb.ID.String()}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l, err := client.Lead.Create().SetTenantID(tid).SetSource("funnel").SetLeadType("customer").SetFunnelID(fa.ID).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	created := &LeadCreatedHandler{db: client, logger: zap.NewNop()}
	if err := created.Handle(ctx, fakeMsg{data: envelope(t, tid, l.ID, map[string]any{"lead_id": l.ID.String(), "lead_type": "customer", "source": "funnel", "funnel_id": fa.ID.String()})}); err != nil {
		t.Fatal(err)
	}
	again := &LeadResubmittedHandler{db: client, logger: zap.NewNop()}
	for i := 0; i < 2; i++ { // the same funnel A again, twice
		if err := again.Handle(ctx, resubmittedMsg(t, tid, l.ID, &fa.ID)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ { // funnel B, delivered twice
		if err := again.Handle(ctx, resubmittedMsg(t, tid, l.ID, &fb.ID)); err != nil {
			t.Fatal(err)
		}
	}
	count := func(seq uuid.UUID) int {
		n, err := client.NurtureEnrollment.Query().Where(nurtureenrollment.LeadID(l.ID), nurtureenrollment.SequenceID(seq)).Count(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(generic.ID) != 1 || count(forB.ID) != 1 {
		t.Fatalf("enrolments: welcome %d, funnel B %d; want 1 and 1", count(generic.ID), count(forB.ID))
	}
}
