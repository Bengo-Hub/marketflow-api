package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/bengobox/marketflow-api/internal/modules/meetings"
)

// A lead from an ad is reported once when it is qualified or converted and once when it books;
// an organic lead never is (owner, 2026-11-25: only sales were sent).
func TestQualifiedLeadsAndBookingsAreReportedOnce(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWorkerPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unavailable")
	}
	defer sqlDB.Close()
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer db.Close()
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Lead conv").SetSlug("lead-conv-" + tid.String()).ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = db.ScheduledMeeting.Delete().Where(scheduledmeeting.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	ad := db.Lead.Create().SetTenantID(tid).SetSource("website_contact").SetLeadType("customer").SetEmail("ad@example.com").
		SetMetadata(map[string]any{"gclid": "G1"}).SaveX(ctx)
	organic := db.Lead.Create().SetTenantID(tid).SetSource("website_contact").SetLeadType("customer").SaveX(ctx)
	ls := leads.NewService(db, zap.NewNop())
	set := func(id uuid.UUID, status string) {
		t.Helper()
		if _, err := ls.Update(ctx, tid, id, leads.UpdateInput{Status: &status}); err != nil {
			t.Fatal(err)
		}
	}
	names := func() map[string]int {
		out := map[string]int{}
		for _, r := range db.OutboxEvent.Query().Where(outboxevent.TenantID(tid), outboxevent.AggregateType("conversion")).AllX(ctx) {
			var ev ConversionEvent
			if err := json.Unmarshal(eventBody(r.Payload), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Gclid != "G1" || ev.Email != "ad@example.com" || ev.ConversionID != r.ID.String() {
				t.Errorf("event %+v", ev)
			}
			out[ev.EventName]++
		}
		return out
	}
	set(ad.ID, "contacted")
	set(organic.ID, "qualified")
	if got := names(); len(got) != 0 {
		t.Fatalf("nothing to report yet: %v", got)
	}
	set(ad.ID, "qualified")
	set(ad.ID, "converted")
	ms := meetings.NewService(db, nil, zap.NewNop())
	for i := 0; i < 2; i++ {
		if _, err := ms.Create(ctx, meetings.CreateInput{TenantID: tid, LeadID: &ad.ID, ScheduledAt: time.Now().Add(time.Hour), SkipCalendarSync: true}); err != nil {
			t.Fatal(err)
		}
	}
	if got := names(); got["Lead"] != 1 || got["Booking"] != 1 || len(got) != 2 {
		t.Fatalf("reported %v", got)
	}
	if metaEventName("Booking") != "Schedule" || metaEventName("Lead") != "Lead" {
		t.Fatal("meta event names")
	}
}
