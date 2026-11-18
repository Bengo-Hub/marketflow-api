package webhooks

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
)

// The public booking page refuses an address that is not a plain, valid one (and an empty or
// overlong name) before anything else runs: no calendar call, no lead, no meeting. The calendar
// part of a booking (slot check, caps, the lead with the notes) needs a connected Google
// Calendar; the caps and the notes rule are tested in the meetings package.
func TestPublicBookingRefusesBadContact(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWebhooksPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	tid := uuid.New()
	slug := "pb-" + tid.String()[:8]
	db.Tenant.Create().SetID(tid).SetName("PB " + slug).SetSlug(slug).SetStatus("active").ExecX(ctx)
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(ctx) })

	// No availability service: a request that got past the contact checks would panic here.
	h := NewPublicHandler(db, nil, nil, nil, nil, nil, nil, zap.NewNop())
	r := chi.NewRouter()
	r.Post("/public/book/{slug}", h.CreateBooking)
	slot := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Hour).Format(time.RFC3339)

	booking := func(name, email string) string {
		return `{"slot_start":"` + slot + `","name":"` + name + `","email":"` + email + `"}`
	}
	cases := []struct{ body, code string }{
		{booking("Jane", "jane@example.com, boss@example.com"), "invalid_email"},
		{booking("Jane", "Jane <jane@example.com>"), "invalid_email"},
		{booking("Jane", "jane@localhost"), "invalid_email"},
		{booking("   ", "jane@example.com"), "invalid_name"},
		{booking(strings.Repeat("J", 121), "jane@example.com"), "invalid_name"},
	}
	for _, c := range cases {
		body, code := c.body, c.code
		req := httptest.NewRequest(http.MethodPost, "/public/book/"+slug, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), code) {
			t.Errorf("%.60s: %d %s", body, rec.Code, rec.Body)
		}
	}
	// An old page's lead_id still decodes (it is ignored, not refused as an unknown field).
	req := httptest.NewRequest(http.MethodPost, "/public/book/"+slug,
		strings.NewReader(`{"slot_start":"`+slot+`","name":"Jane","email":"bad","lead_id":"`+uuid.NewString()+`"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "invalid_email") {
		t.Fatalf("lead_id must still decode: %d %s", rec.Code, rec.Body)
	}

	if n, _ := db.Lead.Query().Where(lead.TenantID(tid)).Count(ctx); n != 0 {
		t.Fatalf("a refused booking made %d leads", n)
	}
	if n, _ := db.ScheduledMeeting.Query().Where(scheduledmeeting.TenantID(tid)).Count(ctx); n != 0 {
		t.Fatalf("a refused booking made %d meetings", n)
	}
}
