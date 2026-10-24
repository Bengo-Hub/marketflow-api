package metastore

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
)

const testDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testDSN)
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

func seed(t *testing.T, db *ent.Client, meta map[string]any) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("M " + tid.String()).SetSlug("m-" + tid.String()).SetMetadata(meta).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(ctx) })
	return tid
}

func TestSetKeysMergesAndRemoves(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seed(t, db, map[string]any{"a": 1, "b": 2})
	if err := SetKeys(ctx, db, tid, map[string]any{"a": nil, "c": "x"}); err != nil {
		t.Fatal(err)
	}
	ten, _ := db.Tenant.Get(ctx, tid)
	if _, has := ten.Metadata["a"]; has || ten.Metadata["b"] != float64(2) || ten.Metadata["c"] != "x" {
		t.Fatalf("metadata = %v", ten.Metadata)
	}
	if err := SetKeys(ctx, db, uuid.New(), map[string]any{"x": 1}); err == nil {
		t.Fatal("unknown tenant must error")
	}
}

// Many writers of different keys at once: none is lost (a whole-map rewrite would drop some).
func TestSetKeysConcurrentWritersKeepEachOthersKeys(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seed(t, db, map[string]any{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := SetKeys(ctx, db, tid, map[string]any{string(rune('a' + i)): i}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	ten, _ := db.Tenant.Get(ctx, tid)
	if len(ten.Metadata) != 12 {
		t.Fatalf("expected 12 keys, got %d: %v", len(ten.Metadata), ten.Metadata)
	}
}
