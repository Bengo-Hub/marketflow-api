package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
)

func TestEventBody(t *testing.T) {
	tid := uuid.New()

	t.Run("envelope returns payload with envelope tenant", func(t *testing.T) {
		raw, _ := eventslib.NewEvent("sale.finalized", "pos", uuid.New(), tid, map[string]any{"order_id": "o1", "total": 150.5}).ToJSON()
		var got map[string]any
		if err := json.Unmarshal(eventBody(raw), &got); err != nil {
			t.Fatal(err)
		}
		if got["order_id"] != "o1" || got["total"] != 150.5 || got["tenant_id"] != tid.String() {
			t.Fatalf("payload not lifted: %+v", got)
		}
		if _, leaked := got["event_type"]; leaked {
			t.Fatal("envelope fields must not leak into the body")
		}
	})
	t.Run("payload tenant wins over envelope", func(t *testing.T) {
		raw, _ := eventslib.NewEvent("x", "pos", uuid.New(), tid, map[string]any{"tenant_id": "from-payload"}).ToJSON()
		var got map[string]any
		_ = json.Unmarshal(eventBody(raw), &got)
		if got["tenant_id"] != "from-payload" {
			t.Fatalf("tenant_id = %v", got["tenant_id"])
		}
	})
	t.Run("flat message passes through unchanged", func(t *testing.T) {
		flat := []byte(`{"tenant_id":"t1","enrollment_id":"e1"}`)
		if string(eventBody(flat)) != string(flat) {
			t.Fatal("flat JSON must be returned as-is")
		}
	})
	t.Run("invalid json passes through for the caller to reject", func(t *testing.T) {
		if string(eventBody([]byte("{"))) != "{" {
			t.Fatal("invalid JSON must be returned unchanged")
		}
	})
	t.Run("flat message with its own payload field is not an envelope", func(t *testing.T) {
		// The shape custom_agents.TriggerRun publishes on crm.agent.run_requested.
		flat := []byte(`{"run_id":"r1","agent_id":"a1","tenant_id":"t1","payload":{"why":"manual"}}`)
		if string(eventBody(flat)) != string(flat) {
			t.Fatal("a message without event_type must pass through unchanged")
		}
	})
	t.Run("non-object payload passes through", func(t *testing.T) {
		raw := []byte(`{"tenant_id":"t1","payload":"text"}`)
		if string(eventBody(raw)) != string(raw) {
			t.Fatal("a string payload is not an envelope body")
		}
	})
}

// pos-api publishes pos.sale.finalized as a shared-events envelope; before eventBody the CRM
// consumer read crm_contact_id at the top level, found nothing and logged no sale, ever.
func TestPOSSaleFinalizedEnvelopeLogsActivity(t *testing.T) {
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
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer db.Close()
	ctx := context.Background()

	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("POS " + tid.String()).SetSlug("pos-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = db.Activity.Delete().Where(activity.TenantID(tid)).Exec(ctx)
		_, _ = db.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	c, err := db.Contact.Create().SetTenantID(tid).SetContactType("customer").Save(ctx)
	if err != nil {
		t.Fatalf("seed contact: %v", err)
	}

	raw, _ := eventslib.NewEvent("sale.finalized", "pos", uuid.New(), tid, map[string]any{
		"order_id": uuid.NewString(), "order_number": "ORD-1", "crm_contact_id": c.ID.String(),
		"total": 1200.0, "currency": "KES",
	}).ToJSON()

	h := &POSSaleFinalizedHandler{db: db, svc: activities.NewService(db, zap.NewNop()), logger: zap.NewNop()}
	if err := h.Handle(ctx, fakeMsg{data: raw}); err != nil {
		t.Fatal(err)
	}
	n, err := db.Activity.Query().Where(activity.TenantID(tid), activity.EntityID(c.ID)).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("activities = %d, want 1 (the POS sale)", n)
	}
}
