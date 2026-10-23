package audit

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/auditlog"
)

func TestTenantRecordsEvent(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable")
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unavailable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer db.Close()
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("Audit " + tid.String()).SetSlug("audit-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.AuditLog.Delete().Where(auditlog.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	sub := uuid.New()
	Tenant(ctx, db, zap.NewNop(), tid, "funnel.published", "funnel", &sub, map[string]any{"slug": "x"})
	Tenant(ctx, db, zap.NewNop(), uuid.Nil, "ignored", "", nil, nil)

	rows, err := db.AuditLog.Query().Where(auditlog.TenantID(tid)).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Event != "funnel.published" || rows[0].Scope != "tenant" || rows[0].Actor == nil || *rows[0].Actor != "system" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}
