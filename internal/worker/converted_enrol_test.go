package worker

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
)

func TestConvertedEnrolOnceRecentOnlyFromTheSale(t *testing.T) {
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
	if _, err := db.Tenant.Create().SetID(tid).SetName("Conv " + tid.String()).SetSlug("conv-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.NurtureEnrollment.Delete().Where(nurtureenrollment.TenantID(tid)).Exec(ctx)
		_, _ = db.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	if _, err := db.NurtureSequence.Create().SetTenantID(tid).SetName("Ask for a testimonial").SetTriggerEvent(ConvertedTrigger).
		SetActive(true).SetSteps([]map[string]any{{"delay_hours": 168, "channel": "email"}}).Save(ctx); err != nil {
		t.Fatalf("seed sequence: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	mk := func(status string, convertedAgo time.Duration) uuid.UUID {
		b := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetStatus(status)
		if convertedAgo > 0 {
			b = b.SetConvertedAt(now.Add(-convertedAgo))
		}
		l, err := b.Save(ctx)
		if err != nil {
			t.Fatalf("seed lead: %v", err)
		}
		return l.ID
	}
	recent := mk("converted", 2*24*time.Hour)
	mk("converted", 90*24*time.Hour) // too old: never emailed
	mk("qualified", 0)                // not a customer

	c := NewConvertedEnrol(db, zap.NewNop())
	n, err := c.enrol(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("first run enrolled %d, err %v", n, err)
	}
	e, err := db.NurtureEnrollment.Query().Where(nurtureenrollment.TenantID(tid)).Only(ctx)
	if err != nil || e.LeadID == nil || *e.LeadID != recent {
		t.Fatalf("enrolment %+v err %v", e, err)
	}
	if e.NextStepAt == nil || !e.NextStepAt.Equal(now.Add(5*24*time.Hour)) {
		t.Fatalf("a week after the sale (2 days ago) is in 5 days, got %v", e.NextStepAt)
	}
	if n, _ := c.enrol(ctx, now); n != 0 {
		t.Fatalf("second run enrolled %d", n)
	}
}
