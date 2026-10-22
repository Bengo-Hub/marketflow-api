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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
)

func TestReEngagementEnrolsDormantLeadsOnce(t *testing.T) {
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
	if _, err := db.Tenant.Create().SetID(tid).SetName("Reeng " + tid.String()).SetSlug("reeng-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = db.NurtureEnrollment.Delete().Where(nurtureenrollment.TenantID(tid)).Exec(ctx)
		_, _ = db.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	seq, err := db.NurtureSequence.Create().SetTenantID(tid).SetName("Win back").SetTriggerEvent("contact.dormant").
		SetActive(true).SetSteps([]map[string]any{}).Save(ctx)
	if err != nil {
		t.Fatalf("seed sequence: %v", err)
	}

	now := time.Now().UTC()
	day := 24 * time.Hour
	mk := func(name string, created, updated time.Duration, status string) uuid.UUID {
		l, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetStatus(status).
			SetFirstName(name).SetCreatedAt(now.Add(-created)).SetUpdatedAt(now.Add(-updated)).Save(ctx)
		if err != nil {
			t.Fatalf("seed lead %s: %v", name, err)
		}
		return l.ID
	}
	dormant := mk("dormant", 90*day, 80*day, "new")
	recent := mk("recent", 10*day, 10*day, "new")
	touched := mk("old but recently updated", 90*day, 5*day, "contacted")
	converted := mk("converted", 90*day, 80*day, "converted")
	done := mk("already went through the sequence", 120*day, 100*day, "new")
	if _, err := db.NurtureEnrollment.Create().SetTenantID(tid).SetSequenceID(seq.ID).SetLeadID(done).
		SetCurrentStep(3).SetStatus("completed").Save(ctx); err != nil {
		t.Fatal(err)
	}

	r := NewReEngagement(db, 24*time.Hour, 60*day, zap.NewNop())
	// Scoped to this test's tenant so seeded local tenants are never touched.
	scope := map[uuid.UUID]uuid.UUID{tid: seq.ID}

	n, err := r.enrolForSequences(ctx, now, scope)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("first run enrolled %d, want exactly 1 (the dormant lead)", n)
	}
	enrolledIn := func(id uuid.UUID) int {
		c, err := db.NurtureEnrollment.Query().Where(nurtureenrollment.LeadID(id), nurtureenrollment.SequenceID(seq.ID)).Count(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if enrolledIn(dormant) != 1 {
		t.Fatal("the dormant lead was not enrolled")
	}
	for name, id := range map[string]uuid.UUID{"recent": recent, "recently updated": touched, "converted": converted} {
		if enrolledIn(id) != 0 {
			t.Fatalf("%s lead must not be enrolled", name)
		}
	}
	if enrolledIn(done) != 1 {
		t.Fatal("a lead that completed the sequence was enrolled again (repeat-send bug)")
	}

	// The next day's run must not enrol anyone again.
	if n, err := r.enrolForSequences(ctx, now.Add(day), scope); err != nil || n != 0 {
		t.Fatalf("second run enrolled %d (err %v), want 0", n, err)
	}
	if got, _ := db.NurtureSequence.Get(ctx, seq.ID); got.TotalEnrolled != 1 {
		t.Fatalf("total_enrolled = %d, want 1", got.TotalEnrolled)
	}
}
