package funnels

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

func openRetentionDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable")
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A live funnel cannot be deleted for good; an archived one can, and its leads stay with the
// funnel's name. The automatic purge waits for the configured window, read per tenant. Everything
// the test creates belongs to its own fresh tenant and is removed by that tenant id only.
func TestFunnelRetention(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("FR " + tid.String()).SetSlug("fr-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_, _ = db.Funnel.Delete().Where(funnel.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	s := NewService(db, zap.NewNop())
	mk := func(name string) *ent.Funnel {
		f, err := db.Funnel.Create().SetTenantID(tid).SetName(name).SetSlug(name + "-" + uuid.NewString()[:8]).Save(ctx)
		if err != nil {
			t.Fatalf("seed funnel: %v", err)
		}
		return f
	}
	f := mk("launch")
	l, err := db.Lead.Create().SetTenantID(tid).SetFunnelID(f.ID).SetFirstName("Amina").Save(ctx)
	if err != nil {
		t.Fatalf("seed lead: %v", err)
	}
	if err := s.Purge(ctx, tid, f.ID); err != ErrNotArchived {
		t.Fatalf("a live funnel must not be purged: %v", err)
	}
	if err := s.Delete(ctx, tid, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Purge(ctx, tid, f.ID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	got, _ := db.Lead.Get(ctx, l.ID)
	if got.FunnelID != nil || got.Metadata[metaLeadFunnelName] != "launch" {
		t.Fatalf("lead must stay, unlinked, with the funnel name: %+v %v", got.FunnelID, got.Metadata)
	}

	// Automatic purge: inside the default 30-day window nothing of this tenant goes; past it, it does.
	g := mk("old")
	if err := s.Delete(ctx, tid, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.purgeArchived(ctx, time.Now(), tid); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.Funnel.Query().Where(funnel.ID(g.ID)).Exist(ctx); !ok {
		t.Fatal("purged inside the window")
	}
	if _, err := s.purgeArchived(ctx, time.Now().Add(31*24*time.Hour), tid); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.Funnel.Query().Where(funnel.ID(g.ID)).Exist(ctx); ok {
		t.Fatal("not purged after the window")
	}
}
