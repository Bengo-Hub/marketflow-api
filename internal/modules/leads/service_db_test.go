package leads

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
)

// Local dev Postgres (PostgreSQL 17, db "marketflow"), same as the contacts tests. Skipped when
// unreachable or when SKIP_POSTGRES_TESTS is set.
const testLeadsPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func newLeadsTestService(t *testing.T) (*Service, *ent.Client) {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testLeadsPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = client.Close() })
	return NewService(client, zap.NewNop()), client
}

func seedLeadsTenant(t *testing.T, client *ent.Client) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	if _, err := client.Tenant.Create().SetID(tid).SetName("Leads Test " + tid.String()).SetSlug("leads-test-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = client.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = client.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = client.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	return tid
}

func TestCreateWritesOneOutboxEventInSameTransaction(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedLeadsTenant(t, client)

	l, err := svc.Create(ctx, CreateInput{TenantID: tid, Source: "website", LeadType: "customer", Email: strp("a@example.com")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rows, err := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid)).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1", len(rows))
	}
	row := rows[0]
	if row.AggregateID != l.ID || row.AggregateType != LeadAggregate || row.EventType != LeadCreatedEvent || row.Status != "PENDING" {
		t.Fatalf("unexpected outbox row %+v", row)
	}
	ev, err := eventslib.FromJSON(row.Payload)
	if err != nil || ev.Subject() != "lead.created" || ev.ID != row.ID {
		t.Fatalf("outbox payload does not rebuild the event: err=%v subject=%q", err, ev.Subject())
	}
}

func TestCreateFailureWritesNoOutboxEvent(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	ghostTenant := uuid.New() // no tenant row: the lead insert fails its FK

	if _, err := svc.Create(ctx, CreateInput{TenantID: ghostTenant, Source: "website", LeadType: "customer"}); err == nil {
		t.Fatal("expected the lead insert to fail for a tenant that does not exist")
	}
	n, err := client.OutboxEvent.Query().Where(outboxevent.TenantID(ghostTenant)).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a failed lead insert left %d outbox rows", n)
	}
}

func TestCreateTwoLeadsTwoEvents(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedLeadsTenant(t, client)
	for i := 0; i < 2; i++ {
		if _, err := svc.Create(ctx, CreateInput{TenantID: tid, Source: "tiktok_ad", LeadType: "customer"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid)).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("outbox rows = %d, want 2", n)
	}
}
