package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
)

const testAdminPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openCostReportDB(t *testing.T) (*ent.Client, *sql.DB) {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testAdminPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db, sqlDB
}

// costTenant creates a tenant with one brief; the tenant, its versions and the brief are removed
// after the test.
func costTenant(t *testing.T, db *ent.Client, name string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName(name).SetSlug("cost-" + tid.String()[:8]).ExecX(ctx)
	b := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Cost report").SaveX(ctx)
	t.Cleanup(func() {
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	return tid, b.ID
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The month's sums per tenant: only versions created inside the month count, a refunded failure
// (no credits, no cost) is not on the report, a revision's copied storyboard is not paid twice,
// a cost that is not a number is ignored, and the tenants page most costly first. The month is
// in 2001 so no other test's data falls inside it.
func TestCostReportAggregation(t *testing.T) {
	db, sqlDB := openCostReportDB(t)
	ctx := context.Background()
	_, start, end, err := monthRange("2001-03", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a, briefA := costTenant(t, db, "Costly Shop")
	b, briefB := costTenant(t, db, "Thrifty Shop")
	no := 0
	version := func(tid, brief uuid.UUID, kind, status string, credits int, at time.Time, meta map[string]any) {
		no++
		c := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(brief).SetVersionNo(no).SetKind(kind).
			SetFormat("1080x1920").SetStatus(status).SetCredits(credits).SetCreatedAt(at)
		if meta != nil {
			c.SetMetadata(meta)
		}
		c.ExecX(ctx)
	}
	day := func(d, h int) time.Time { return time.Date(2001, 3, d, h, 0, 0, 0, time.UTC) }

	// Tenant A: a poster, a video ad with its script and storyboard, and a revision that carries
	// the same storyboard (made before the revision existed).
	version(a, briefA, "poster", "approved", 2, day(5, 9), map[string]any{"cost_usd": 0.05, "route": "fal"})
	version(a, briefA, "video_ad", "ready", 30, day(10, 9), map[string]any{"cost_usd": 1.5, "script_cost_usd": 0.01,
		"storyboard": map[string]any{"cost_usd": 0.03, "made_at": "2001-03-10T10:00:00Z"}})
	version(a, briefA, "video_ad", "ready", 20, day(12, 9), map[string]any{"cost_usd": 1.0, "revision_of": "x",
		"storyboard": map[string]any{"cost_usd": 0.03, "made_at": "2001-03-10T10:00:00Z"}})
	// A refunded failed poster: nothing charged, nothing recorded.
	version(a, briefA, "poster", "failed", 0, day(6, 9), map[string]any{"attempts": 1})
	// Outside the month on either side.
	version(a, briefA, "poster", "ready", 99, time.Date(2001, 4, 1, 0, 0, 0, 0, time.UTC), map[string]any{"cost_usd": 9.0})
	version(a, briefA, "poster", "ready", 77, time.Date(2001, 2, 28, 23, 59, 59, 0, time.UTC), map[string]any{"cost_usd": 7.0})

	// Tenant B: a video ad whose render failed after its script (the script's credits stand,
	// the render was refunded), and a poster whose cost is not a number.
	version(b, briefB, "video_ad", "failed", 1, time.Date(2001, 3, 31, 23, 59, 59, 0, time.UTC), map[string]any{"script_cost_usd": 0.002})
	version(b, briefB, "poster", "ready", 2, day(1, 0), map[string]any{"cost_usd": "abc"})

	tot, rows, err := queryCostSums(ctx, sqlDB, start, end, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if tot.Tenants != 2 || tot.Versions != 5 || tot.Credits != 55 || !near(tot.CostUSD, 2.592) {
		t.Fatalf("totals = %+v, want 2 tenants, 5 versions, 55 credits, USD 2.592", tot)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if r := rows[0]; r.TenantID != a || r.Name != "Costly Shop" || r.Versions != 3 || r.Credits != 52 || !near(r.CostUSD, 2.59) {
		t.Fatalf("tenant A = %+v, want 3 versions, 52 credits, USD 2.59", r)
	}
	if r := rows[1]; r.TenantID != b || r.Versions != 2 || r.Credits != 3 || !near(r.CostUSD, 0.002) {
		t.Fatalf("tenant B = %+v, want 2 versions, 3 credits, USD 0.002", r)
	}

	// Pages of one: A, then B, then nothing; the totals never change with the page.
	for page, want := range map[int]uuid.UUID{1: a, 2: b} {
		tot, rows, err := queryCostSums(ctx, sqlDB, start, end, page, 1)
		if err != nil || len(rows) != 1 || rows[0].TenantID != want || tot.Tenants != 2 || tot.Credits != 55 {
			t.Fatalf("page %d = %+v %+v (%v)", page, rows, tot, err)
		}
	}
	if tot, rows, err := queryCostSums(ctx, sqlDB, start, end, 3, 1); err != nil || len(rows) != 0 || tot.Tenants != 2 {
		t.Fatalf("page 3 = %+v %+v (%v)", rows, tot, err)
	}

	// Through the handler: treasury's rate, the JSON contract and the paging.
	h := NewCostReportHandler(db, sqlDB, func(context.Context, string) (float64, error) { return 130, nil }, zap.NewNop())
	w := httptest.NewRecorder()
	h.CostReport(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/platform/cost-report?month=2001-03&page=1&page_size=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Month       string             `json:"month"`
		FX          float64            `json:"fx_kes_per_usd"`
		FXSource    string             `json:"fx_source"`
		CreditValue float64            `json:"credit_value_kes"`
		Totals      map[string]any     `json:"totals"`
		Tenants     []map[string]any   `json:"tenants"`
		Pagination  map[string]float64 `json:"pagination"`
		Gaps        []string           `json:"gaps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Month != "2001-03" || got.FX != 130 || got.FXSource != "treasury" || got.CreditValue <= 0 || len(got.Gaps) != 2 {
		t.Fatalf("report = %+v", got)
	}
	if got.Pagination["page"] != 1 || got.Pagination["page_size"] != 1 || got.Pagination["total"] != 2 {
		t.Fatalf("pagination = %+v", got.Pagination)
	}
	if len(got.Tenants) != 1 || got.Tenants[0]["tenant_id"] != a.String() || got.Tenants[0]["credits_charged"] != float64(52) {
		t.Fatalf("tenants = %+v", got.Tenants)
	}
	for _, k := range []string{"versions", "credits_charged", "credits_kes", "media_cost_usd", "media_cost_kes", "margin", "below_floor"} {
		if _, ok := got.Totals[k]; !ok {
			t.Errorf("totals has no %q", k)
		}
	}
	if !near(got.Totals["media_cost_kes"].(float64), round(2.592*130, 2)) {
		t.Fatalf("totals media_cost_kes = %v", got.Totals["media_cost_kes"])
	}

	// Treasury down: the registry's rate, and a bad month or page is a 400.
	h = NewCostReportHandler(db, sqlDB, func(context.Context, string) (float64, error) { return 0, sql.ErrNoRows }, zap.NewNop())
	w = httptest.NewRecorder()
	h.CostReport(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/platform/cost-report?month=2001-03", nil))
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.FXSource != "registry" || got.FX <= 0 {
		t.Fatalf("treasury down: %d %s", w.Code, w.Body.String())
	}
	for _, q := range []string{"?month=2001-3", "?page=0", "?page_size=500"} {
		w = httptest.NewRecorder()
		h.CostReport(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/platform/cost-report"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, w.Code)
		}
	}
}
