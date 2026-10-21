package worker

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
)

// envelope builds a lead.created message exactly as the leads outbox emits it.
func envelope(t *testing.T, tenantID, leadID uuid.UUID, payload map[string]any) []byte {
	t.Helper()
	raw, err := eventslib.NewEvent(leads.LeadCreatedEvent, leads.LeadAggregate, leadID, tenantID, payload).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeLeadCreated(t *testing.T) {
	tid, lid := uuid.New(), uuid.New()

	t.Run("outbox envelope", func(t *testing.T) {
		ev, err := decodeLeadCreated(envelope(t, tid, lid, map[string]any{"lead_id": lid.String(), "lead_type": "recruit", "source": "meta_ad"}))
		if err != nil {
			t.Fatal(err)
		}
		if ev.TenantID != tid || ev.LeadID != lid || ev.LeadType != "recruit" || ev.Source != "meta_ad" {
			t.Fatalf("decoded %+v", ev)
		}
	})
	t.Run("lead id falls back to aggregate id", func(t *testing.T) {
		ev, err := decodeLeadCreated(envelope(t, tid, lid, map[string]any{"source": "organic"}))
		if err != nil || ev.LeadID != lid {
			t.Fatalf("err=%v lead=%s, want %s", err, ev.LeadID, lid)
		}
	})
	t.Run("missing tenant is rejected", func(t *testing.T) {
		if _, err := decodeLeadCreated(envelope(t, uuid.Nil, lid, map[string]any{"lead_id": lid.String()})); err == nil {
			t.Fatal("expected an error for a missing tenant")
		}
	})
	t.Run("invalid lead id is rejected", func(t *testing.T) {
		if _, err := decodeLeadCreated(envelope(t, tid, lid, map[string]any{"lead_id": "not-a-uuid"})); err == nil {
			t.Fatal("expected an error for a malformed lead_id")
		}
	})
	t.Run("old flat shape is rejected, not misread", func(t *testing.T) {
		flat := []byte(`{"tenant_id":"` + tid.String() + `","lead_id":"` + lid.String() + `"}`)
		if _, err := decodeLeadCreated(flat); err == nil {
			t.Fatal("a flat message has no aggregate id or payload and must not decode as a lead")
		}
	})
	t.Run("garbage is rejected", func(t *testing.T) {
		if _, err := decodeLeadCreated([]byte("{")); err == nil {
			t.Fatal("expected a JSON error")
		}
	})
}

func TestLeadScorerFollowsLeadsTopic(t *testing.T) {
	if got := (&LeadScorerAgent{}).Subject(); got != "lead.created" {
		t.Fatalf("scorer subject = %q, want lead.created (the subject the leads outbox publishes)", got)
	}
	if got := (&LeadCreatedHandler{}).Subject(); got != leads.LeadCreatedTopic {
		t.Fatalf("nurture handler subject = %q, want %q", got, leads.LeadCreatedTopic)
	}
}

// fakeMsg serves only Data; any other call would panic and fail the test, proving the happy
// path never acks, naks or terms on its own.
type fakeMsg struct {
	jetstream.Msg
	data []byte
}

func (m fakeMsg) Data() []byte { return m.data }

const testWorkerPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func TestLeadCreatedHandlerEnrolsOnceOnRedelivery(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWorkerPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	defer sqlDB.Close()
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer client.Close()
	ctx := context.Background()

	tid := uuid.New()
	if _, err := client.Tenant.Create().SetID(tid).SetName("Worker Test " + tid.String()).SetSlug("worker-test-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = client.NurtureEnrollment.Delete().Where(nurtureenrollment.TenantID(tid)).Exec(ctx)
		_, _ = client.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
		_, _ = client.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = client.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	seq, err := client.NurtureSequence.Create().SetTenantID(tid).SetName("Welcome").SetTriggerEvent("lead.created").
		SetActive(true).SetSteps([]map[string]any{}).Save(ctx)
	if err != nil {
		t.Fatalf("seed sequence: %v", err)
	}
	l, err := client.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").Save(ctx)
	if err != nil {
		t.Fatalf("seed lead: %v", err)
	}

	h := &LeadCreatedHandler{db: client, logger: zap.NewNop()}
	msg := fakeMsg{data: envelope(t, tid, l.ID, map[string]any{"lead_id": l.ID.String(), "lead_type": "customer", "source": "website"})}
	for i := 0; i < 3; i++ { // first delivery plus two redeliveries
		if err := h.Handle(ctx, msg); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	n, err := client.NurtureEnrollment.Query().Where(
		nurtureenrollment.TenantID(tid), nurtureenrollment.LeadID(l.ID), nurtureenrollment.SequenceID(seq.ID),
	).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("enrollments = %d after 3 deliveries, want exactly 1", n)
	}
}
