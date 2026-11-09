package endpoints

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
)

const testPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

// The store keeps one platform row per service, and an approval that lands while a scan is
// between its read and its write is not lost: the scan's write is retried on top of it.
func TestEntStoreUpdateDoesNotLoseAConcurrentWrite(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
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
	const svc = "zz-test-endpoints"
	cleanup := func() {
		_, _ = db.ServiceConfig.Delete().Where(serviceconfig.ConfigKeyEQ(ConfigPrefix + svc)).Exec(ctx)
	}
	cleanup()
	t.Cleanup(cleanup)

	st := EntStore{DB: db}
	route := Endpoint{Method: "GET", Path: "/api/v1/{tenant}/promotions/deals", Status: StatusCandidate}
	if err := st.Update(ctx, svc, func([]Endpoint) ([]Endpoint, error) { return []Endpoint{route}, nil }); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The "scan" reads, then an approval lands, then the scan writes: its first write must fail
	// the updated_at check and the retry must see the approval.
	calls := 0
	err = st.Update(ctx, svc, func(list []Endpoint) ([]Endpoint, error) {
		calls++
		if calls == 1 {
			if err := st.Update(ctx, svc, func(l []Endpoint) ([]Endpoint, error) {
				return Decide(l, route.Method, route.Path, StatusApproved, "owner", route.FirstSeen)
			}); err != nil {
				t.Fatalf("approve: %v", err)
			}
		}
		return append(list, Endpoint{Method: "GET", Path: "/api/v1/{tenant}/promotions/banners", Status: StatusCandidate}), nil
	})
	if err != nil {
		t.Fatalf("scan update: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the conflicting write should have been retried once, calls=%d", calls)
	}
	got, err := st.Load(ctx, svc)
	if err != nil || len(got) != 2 || got[0].Status != StatusApproved {
		t.Fatalf("lost the approval: %+v, %v", got, err)
	}
	all, err := st.All(ctx)
	if err != nil || len(all[svc]) != 2 {
		t.Fatalf("All = %v, %v", all[svc], err)
	}
}
