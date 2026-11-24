package activities

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
)

const testActivitiesPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

// A call or email a person logs on a new lead marks it contacted (owner, 2026-11-24); a note, an
// inbound message or an automated entry does not.
func TestLoggedContactMarksLeadContacted(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testActivitiesPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	defer sqlDB.Close()
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("Act " + tid.String()).SetSlug("act-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Activity.Delete().Where(activity.TenantID(tid)).Exec(ctx)
		_, _ = db.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	svc := NewService(db, zap.NewNop())
	mk := func() uuid.UUID {
		l, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return l.ID
	}
	status := func(id uuid.UUID) string {
		l, _ := db.Lead.Get(ctx, id)
		return l.Status
	}
	by := uuid.New()
	inbound := "inbound"
	note, auto, in, call := mk(), mk(), mk(), mk()
	for _, c := range []struct {
		id  uuid.UUID
		in  CreateInput
		was string
	}{
		{note, CreateInput{ActivityType: "note", PerformedBy: &by}, "new"},
		{auto, CreateInput{ActivityType: "email"}, "new"},
		{in, CreateInput{ActivityType: "whatsapp", Direction: &inbound, PerformedBy: &by}, "new"},
		{call, CreateInput{ActivityType: "call", PerformedBy: &by}, "contacted"},
	} {
		c.in.TenantID, c.in.EntityType, c.in.EntityID = tid, "lead", c.id
		if _, err := svc.Log(ctx, c.in); err != nil {
			t.Fatal(err)
		}
		if got := status(c.id); got != c.was {
			t.Fatalf("%s: status %s, want %s", c.in.ActivityType, got, c.was)
		}
	}
}
