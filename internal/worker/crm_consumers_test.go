package worker

import (
	"context"
	"testing"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
)

func TestEventIDOf(t *testing.T) {
	ev := eventslib.NewEvent("sale.finalized", "pos", uuid.New(), uuid.New(), map[string]any{"order_id": "o-1"})
	raw, err := ev.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if got := eventIDOf(raw); got != ev.ID.String() {
		t.Fatalf("envelope id = %q, want %q", got, ev.ID)
	}
	bare := []byte(`{"order_id":"o-1"}`)
	if eventIDOf(bare) != eventIDOf(bare) || eventIDOf(bare) == eventIDOf([]byte(`{"order_id":"o-2"}`)) {
		t.Fatal("a bare payload must hash to a stable, distinct id")
	}
}

func posSaleEvent(t *testing.T, tenantID, contactID uuid.UUID, orderID string) []byte {
	t.Helper()
	raw, err := eventslib.NewEvent("sale.finalized", "pos", uuid.New(), tenantID, map[string]any{
		"order_id":       orderID,
		"order_number":   "POS-" + orderID,
		"crm_contact_id": contactID.String(),
		"total":          1250.0,
		"currency":       "KES",
	}).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A redelivered sale, or the same sale published again under a fresh envelope, is one entry.
func TestPOSSaleFinalizedHandlerLogsOnce(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db, "crm-pos-sale")
	t.Cleanup(func() { _, _ = db.Activity.Delete().Where(activity.TenantID(tenantID)).Exec(ctx) })
	contactID := uuid.New()
	h := &POSSaleFinalizedHandler{db: db, svc: activities.NewService(db, zap.NewNop()), logger: zap.NewNop()}

	first := posSaleEvent(t, tenantID, contactID, "o-1")
	for _, data := range [][]byte{first, first, posSaleEvent(t, tenantID, contactID, "o-1")} {
		if err := h.Handle(ctx, fakeMsg{data: data}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Handle(ctx, fakeMsg{data: posSaleEvent(t, tenantID, contactID, "o-2")}); err != nil {
		t.Fatal(err)
	}

	rows := db.Activity.Query().Where(activity.TenantID(tenantID), activity.EntityID(contactID)).AllX(ctx)
	if len(rows) != 2 {
		t.Fatalf("want 2 entries (o-1, o-2), got %d", len(rows))
	}
	for _, a := range rows {
		if a.Metadata[activityEventIDKey] == nil || a.Metadata["event_subject"] != "pos.sale.finalized" {
			t.Fatalf("metadata %v", a.Metadata)
		}
	}
}

// The same order id from two producers stays two entries: the source is part of the key.
func TestPurchaseConsumersKeyBySource(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db, "crm-purchase-source")
	t.Cleanup(func() { _, _ = db.Activity.Delete().Where(activity.TenantID(tenantID)).Exec(ctx) })
	contactID := uuid.New()
	svc := activities.NewService(db, zap.NewNop())

	ordering := &OrderingOrderCreatedHandler{db: db, svc: svc, logger: zap.NewNop()}
	raw, err := eventslib.NewEvent("order.created", "ordering", uuid.New(), tenantID, map[string]any{
		"order_id": "o-1", "order_number": "ORD-1", "crm_contact_id": contactID.String(), "total": 10.0, "currency": "KES",
	}).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ordering.Handle(ctx, fakeMsg{data: raw}); err != nil {
			t.Fatal(err)
		}
	}
	pos := &POSSaleFinalizedHandler{db: db, svc: svc, logger: zap.NewNop()}
	if err := pos.Handle(ctx, fakeMsg{data: posSaleEvent(t, tenantID, contactID, "o-1")}); err != nil {
		t.Fatal(err)
	}

	if n := db.Activity.Query().Where(activity.TenantID(tenantID), activity.EntityID(contactID)).CountX(ctx); n != 2 {
		t.Fatalf("want one ordering and one POS entry, got %d", n)
	}
}
