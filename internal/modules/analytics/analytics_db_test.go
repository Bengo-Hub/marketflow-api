package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/nurturedeliveryevent"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/ent/profileevent"
)

const testAnalyticsPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testAnalyticsPostgresDSN)
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

func seedTenant(t *testing.T, db *ent.Client) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("An " + tid.String()).SetSlug("an-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ProfileEvent.Delete().Where(profileevent.TenantID(tid)).Exec(ctx)
		_, _ = db.NurtureDeliveryEvent.Delete().Where(nurturedeliveryevent.TenantID(tid)).Exec(ctx)
		_, _ = db.NurtureEnrollment.Delete().Where(nurtureenrollment.TenantID(tid)).Exec(ctx)
		_, _ = db.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	return tid
}

func call(t *testing.T, h http.HandlerFunc, tid uuid.UUID, path string, params map[string]string) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Tenant-ID", tid.String())
	if len(params) > 0 {
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
	}
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func TestProfileMetricsCountsInDatabase(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db)
	now := time.Now().UTC()
	for _, e := range []struct{ typ, ip string }{{"view", "a"}, {"view", "a"}, {"view", "b"}, {"click", "c"}, {"click", ""}} {
		if _, err := db.ProfileEvent.Create().SetTenantID(tid).SetSlug("s").SetEventType(e.typ).SetIPHash(e.ip).SetCreatedAt(now.Add(-time.Hour)).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Outside the 30-day window.
	if _, err := db.ProfileEvent.Create().SetTenantID(tid).SetSlug("s").SetEventType("view").SetIPHash("old").SetCreatedAt(now.AddDate(0, 0, -40)).Save(ctx); err != nil {
		t.Fatal(err)
	}
	out := call(t, NewHandler(db, zap.NewNop()).ProfileMetrics, tid, "/analytics/profile", nil)
	counts := out["counts"].(map[string]any)
	if counts["view"].(float64) != 3 || counts["click"].(float64) != 2 || out["total_events"].(float64) != 5 || out["unique_visitors"].(float64) != 3 {
		t.Fatalf("profile metrics = %+v", out)
	}
}

func TestSequenceMetricsGroupedByEnrollmentJoin(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db)
	seq, err := db.NurtureSequence.Create().SetTenantID(tid).SetName("Welcome").SetTriggerEvent("lead.created").SetSteps([]map[string]any{}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	otherSeq, _ := db.NurtureSequence.Create().SetTenantID(tid).SetName("Other").SetTriggerEvent("x").SetSteps([]map[string]any{}).Save(ctx)
	enrol := func(s *ent.NurtureSequence, status string) *ent.NurtureEnrollment {
		lid := uuid.New()
		e, err := db.NurtureEnrollment.Create().SetTenantID(tid).SetSequenceID(s.ID).SetLeadID(lid).SetStatus(status).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e1, e2 := enrol(seq, "active"), enrol(seq, "completed")
	eOther := enrol(otherSeq, "active")
	for _, x := range []struct {
		e   *ent.NurtureEnrollment
		typ string
	}{{e1, "sent"}, {e1, "delivered"}, {e2, "sent"}, {e2, "opened"}, {eOther, "sent"}} {
		if _, err := db.NurtureDeliveryEvent.Create().SetTenantID(tid).SetEnrollmentID(x.e.ID).SetChannel("email").SetEventType(x.typ).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	out := call(t, NewHandler(db, zap.NewNop()).SequenceMetrics, tid, "/analytics/sequences/x", map[string]string{"id": seq.ID.String()})
	counts := out["counts"].(map[string]any)
	if counts["sent"].(float64) != 2 || counts["delivered"].(float64) != 1 || counts["opened"].(float64) != 1 {
		t.Fatalf("counts = %+v (the other sequence's event must not count)", counts)
	}
	enr := out["enrollments"].(map[string]any)
	if enr["total"].(float64) != 2 || enr["completed"].(float64) != 1 {
		t.Fatalf("enrollments = %+v", enr)
	}
}
