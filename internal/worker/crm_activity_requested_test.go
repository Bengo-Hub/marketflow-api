package worker

import (
	"context"
	"testing"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
)

// ticketActivityEvent builds the message exactly as ticketing-api's outbox emits it:
// aggregate "crm", event type "activity.log_requested", the ticket's tenant on the envelope.
func ticketActivityEvent(t *testing.T, tenantID, contactID uuid.UUID, activityType, ticketID string) []byte {
	t.Helper()
	ev := eventslib.NewEvent("activity.log_requested", "crm", uuid.New(), tenantID, map[string]any{
		"entity_type":   "contact",
		"entity_id":     contactID.String(),
		"activity_type": activityType,
		"external_ref":  ticketID,
		"subject":       "Cannot process order",
		"source":        "ticketing-api",
	})
	if got := ev.Subject(); got != CRMActivityRequestedSubject {
		t.Fatalf("subject = %q, want %q", got, CRMActivityRequestedSubject)
	}
	raw, err := ev.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeCRMActivityRequested(t *testing.T) {
	tenantID, contactID := uuid.New(), uuid.New()
	raw := ticketActivityEvent(t, tenantID, contactID, "ticket_resolved", "t-1")
	r, err := decodeCRMActivityRequested(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.tenantID != tenantID || r.entityID != contactID || r.entity != "contact" {
		t.Fatalf("decoded %+v", r)
	}
	in := r.input()
	if in.ActivityType != "ticket_resolved" || in.Outcome == nil || *in.Outcome != "resolved" {
		t.Fatalf("input %+v", in)
	}
	if in.Source == nil || *in.Source != "ticketing-api" || in.ExternalRef == nil || *in.ExternalRef != "t-1" {
		t.Fatalf("source/ref %+v", in)
	}
	if in.Metadata[activityEventIDKey] != r.eventID || r.eventID == uuid.Nil.String() {
		t.Fatalf("event id not carried: %v", in.Metadata)
	}

	again, _ := decodeCRMActivityRequested(raw)
	if again.eventID != r.eventID {
		t.Fatal("same message must decode to the same event id")
	}
}

func TestDecodeCRMActivityRequestedRejectsBadRequests(t *testing.T) {
	tenantID := uuid.New()
	cases := map[string]map[string]any{
		"entity":   {"entity_type": "ticket", "entity_id": uuid.NewString(), "activity_type": "x"},
		"id":       {"entity_type": "contact", "entity_id": "nope", "activity_type": "x"},
		"activity": {"entity_type": "contact", "entity_id": uuid.NewString()},
	}
	for name, payload := range cases {
		raw, _ := eventslib.NewEvent("activity.log_requested", "crm", uuid.New(), tenantID, payload).ToJSON()
		if _, err := decodeCRMActivityRequested(raw); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	raw, _ := eventslib.NewEvent("activity.log_requested", "crm", uuid.New(), uuid.Nil,
		map[string]any{"entity_type": "contact", "entity_id": uuid.NewString(), "activity_type": "x"}).ToJSON()
	if _, err := decodeCRMActivityRequested(raw); err == nil {
		t.Error("missing tenant: want an error")
	}
}

// A redelivered ticket event must not add a second timeline entry.
func TestCRMActivityRequestedHandlerLogsOnceOnRedelivery(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, db, "crm-activity")
	t.Cleanup(func() { _, _ = db.Activity.Delete().Where(activity.TenantID(tenantID)).Exec(ctx) })
	contactID := uuid.New()
	h := &CRMActivityRequestedHandler{db: db, svc: activities.NewService(db, zap.NewNop()), logger: zap.NewNop()}

	opened := ticketActivityEvent(t, tenantID, contactID, "ticket_opened", "t-9")
	for i := 0; i < 2; i++ {
		if err := h.Handle(ctx, fakeMsg{data: opened}); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	// A second, distinct event for the same ticket (resolved) is its own entry.
	if err := h.Handle(ctx, fakeMsg{data: ticketActivityEvent(t, tenantID, contactID, "ticket_resolved", "t-9")}); err != nil {
		t.Fatal(err)
	}

	rows := db.Activity.Query().Where(activity.TenantID(tenantID), activity.EntityID(contactID)).AllX(ctx)
	if len(rows) != 2 {
		t.Fatalf("want 2 timeline entries (opened, resolved), got %d", len(rows))
	}
	for _, a := range rows {
		if a.EntityType != "contact" || a.ExternalRef == nil || *a.ExternalRef != "t-9" {
			t.Fatalf("activity %+v", a)
		}
		if time.Since(a.PerformedAt) > time.Hour {
			t.Fatalf("performed_at should be the event time, got %v", a.PerformedAt)
		}
	}
}
