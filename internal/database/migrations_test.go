package database

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	atlasmigrate "ariga.io/atlas/sql/migrate"

	"github.com/bengobox/marketflow-api/internal/ent"
)

const testPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

// Once the schema is migrated, running the migration again must plan nothing. Postgres stores a
// partial index predicate in its own canonical form, and Atlas compares predicates as text, so an
// IndexWhere written any other way makes every startup drop and rebuild that index under an
// ACCESS EXCLUSIVE lock on its table, which deadlocks with live traffic (and with tests running
// in parallel against the same database).
func TestRunMigrationsIsIdempotent(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	if err := RunMigrations(ctx, client); err != nil {
		t.Fatal(err)
	}
	var planned []string
	// Record the second run's plan instead of applying it.
	record := schema.WithApplyHook(func(schema.Applier) schema.Applier {
		return schema.ApplyFunc(func(_ context.Context, _ dialect.ExecQuerier, plan *atlasmigrate.Plan) error {
			for _, c := range plan.Changes {
				planned = append(planned, c.Cmd)
			}
			return nil
		})
	})
	if err := client.Schema.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	if len(planned) > 0 {
		t.Fatalf("a migrated schema still plans %d changes:\n%s", len(planned), strings.Join(planned, "\n"))
	}
}
