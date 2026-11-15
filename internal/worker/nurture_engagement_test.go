package worker

import (
	"context"
	"testing"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/nurturedeliveryevent"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
)

// engagementEvent builds the message as notifications-api's engagement package publishes it.
func engagementEvent(t *testing.T, tenantID uuid.UUID, enrollment, eventType string) []byte {
	t.Helper()
	raw, err := eventslib.NewEvent("engagement.recorded", "notification", uuid.New(), tenantID, map[string]any{
		"tenant_id": tenantID.String(), "enrollment_id": enrollment, "step_index": 1,
		"channel": "whatsapp", "event_type": eventType, "provider_message_id": "wamid.1",
		"metadata": map[string]any{"sequence_id": "s1", "source": "marketflow-worker"},
	}).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeNurtureEngagement(t *testing.T) {
	tid := uuid.New()
	raw := engagementEvent(t, tid, uuid.NewString(), "OPENED")
	v, id, err := decodeNurtureEngagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	if v.Tenant != tid || v.EventType != "opened" || v.StepIndex != 1 || v.Skip || id == "" {
		t.Fatalf("decoded %+v id %q", v, id)
	}
	if _, _, err := decodeNurtureEngagement(engagementEvent(t, tid, uuid.NewString(), "teleported")); err == nil {
		t.Fatal("unknown event type: want an error")
	}
	v, _, err = decodeNurtureEngagement(engagementEvent(t, tid, "", "sent"))
	if err != nil || !v.Skip {
		t.Fatalf("no enrollment is skipped, got %+v %v", v, err)
	}
}

func TestNurtureEngagementHandlerRecordsOnce(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "nurture-engagement")
	seq := db.NurtureSequence.Create().SetTenantID(tid).SetName("Welcome").SetTriggerEvent("lead.created").
		SetActive(true).SetSteps([]map[string]any{}).SaveX(ctx)
	l := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SaveX(ctx)
	enr := db.NurtureEnrollment.Create().SetTenantID(tid).SetSequenceID(seq.ID).SetLeadID(l.ID).SaveX(ctx)
	t.Cleanup(func() {
		_, _ = db.NurtureDeliveryEvent.Delete().Where(nurturedeliveryevent.TenantID(tid)).Exec(ctx)
		_, _ = db.NurtureEnrollment.Delete().Where(nurtureenrollment.TenantID(tid)).Exec(ctx)
		_, _ = db.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
	})
	h := &NurtureEngagementHandler{db: db, logger: zap.NewNop()}

	read := engagementEvent(t, tid, enr.ID.String(), "opened")
	for i := 0; i < 2; i++ {
		if err := h.Handle(ctx, fakeMsg{data: read}); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	if err := h.Handle(ctx, fakeMsg{data: engagementEvent(t, tid, enr.ID.String(), "replied")}); err != nil {
		t.Fatal(err)
	}
	rows := db.NurtureDeliveryEvent.Query().Where(nurturedeliveryevent.EnrollmentID(enr.ID)).AllX(ctx)
	if len(rows) != 2 {
		t.Fatalf("want opened and replied once each, got %d rows", len(rows))
	}
}
