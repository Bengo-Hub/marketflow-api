package contacts

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
)

// testMarketflowPostgresDSN is the local dev Postgres instance (PostgreSQL 17, db "marketflow",
// user/password "postgres") — mirrors the pattern in treasury-api's balance_events_test.go. This
// service uses ent's typed query API throughout, so a real database connection is the only way to
// exercise the actual dedup/constraint-race behaviour end-to-end (a fake can't reproduce a real
// unique-index violation).
const testMarketflowPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func newContactsTestService(t *testing.T) (*Service, *ent.Client) {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testMarketflowPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable (expected at %s): %v", testMarketflowPostgresDSN, err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	client := ent.NewClient(ent.Driver(drv))
	t.Cleanup(func() { _ = client.Close() })

	return NewService(client, zap.NewNop()), client
}

func strPtr(s string) *string { return &s }

// seedTenant creates the minimal Tenant row Contact.tenant_id's required FK edge needs — Contact
// cannot exist without an owning Tenant.
func seedTenant(t *testing.T, client *ent.Client, tid uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := client.Tenant.Create().
		SetID(tid).SetName("Test Tenant " + tid.String()).SetSlug("test-" + tid.String()).
		Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() { _ = client.Tenant.DeleteOneID(tid).Exec(context.Background()) })
}

// TestCreate_DedupsByNormalizedPhone_FormatAgnostic proves the format-agnostic dedup: the same
// phone entered in three different formats resolves to ONE contact, not three.
func TestCreate_DedupsByNormalizedPhone_FormatAgnostic(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() { _, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(context.Background()) })

	first, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("0712345678"), ContactType: "customer"})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("+254 712 345 678"), ContactType: "customer"})
	if err != nil {
		t.Fatalf("second Create (different format, same number): %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("expected the same contact for two formats of the same phone, got %s and %s", first.ID, second.ID)
	}

	count, err := client.Contact.Query().Where(contact.TenantID(tid)).Count(ctx)
	if err != nil {
		t.Fatalf("count contacts: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 contact row, got %d", count)
	}
}

// TestCreate_DedupsByNormalizedEmail_CaseAgnostic proves email dedup ignores case/whitespace.
func TestCreate_DedupsByNormalizedEmail_CaseAgnostic(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() { _, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(context.Background()) })

	first, err := svc.Create(ctx, CreateInput{TenantID: tid, Email: strPtr("Jane@Example.com"), ContactType: "customer"})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := svc.Create(ctx, CreateInput{TenantID: tid, Email: strPtr("  jane@example.com  "), ContactType: "customer"})
	if err != nil {
		t.Fatalf("second Create (different case/whitespace, same email): %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("expected the same contact for two formats of the same email, got %s and %s", first.ID, second.ID)
	}
}

// TestCreate_ConcurrentSamePhone_NoDuplicateRow is the regression test for the actual root-cause
// bug: Service.Create's dedup was a check-then-act (Query then Create, no DB constraint) — two
// concurrent calls for the SAME phone could both pass the read and both insert. The partial
// unique index on (tenant_id, phone_subscriber_digits) plus the constraint-catch-then-refetch in
// Create must make this genuinely atomic: exactly one contact survives, and NEITHER caller errors.
func TestCreate_ConcurrentSamePhone_NoDuplicateRow(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() { _, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(context.Background()) })

	const phone = "+254700111222"
	const n = 5
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr(phone), ContactType: "customer"})
			errs[i] = err
			if c != nil {
				ids[i] = c.ID
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Create call %d failed: %v", i, err)
		}
	}
	firstID := ids[0]
	for i, id := range ids {
		if id != firstID {
			t.Errorf("call %d returned contact %s, want the same contact as call 0 (%s)", i, id, firstID)
		}
	}

	count, err := client.Contact.Query().Where(contact.TenantID(tid)).Count(ctx)
	if err != nil {
		t.Fatalf("count contacts: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 contact row after %d concurrent creates for the same phone, got %d", n, count)
	}
}

// TestUpdate_RecomputesNormalizedColumns proves a manual edit keeps email_normalized/
// phone_subscriber_digits in sync with the raw fields, so a later Create's dedup check (or the
// unique index) never operates on a stale value.
func TestUpdate_RecomputesNormalizedColumns(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() { _, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(context.Background()) })

	c, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("0700000001"), ContactType: "customer"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.Update(ctx, tid, c.ID, UpdateInput{Phone: strPtr("0700000002")}); err != nil {
		t.Fatalf("Update phone: %v", err)
	}

	// A subsequent Create for the OLD phone must no longer resolve to this contact.
	other, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("0700000001"), ContactType: "customer"})
	if err != nil {
		t.Fatalf("Create with the now-vacated old phone: %v", err)
	}
	if other.ID == c.ID {
		t.Error("expected a NEW contact for the old (vacated) phone, got the same one back — normalized column was not recomputed on Update")
	}

	// And the new phone must now dedup correctly.
	again, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("+254 700 00 00 02")})
	if err != nil {
		t.Fatalf("Create with the new phone (different format): %v", err)
	}
	if again.ID != c.ID {
		t.Errorf("expected the new phone to dedup back to the updated contact %s, got %s", c.ID, again.ID)
	}
}
