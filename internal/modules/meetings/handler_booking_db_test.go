package meetings

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
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
)

const testMeetingsPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

// A service key booking for a visitor (marketflow-ai's book_meeting tool) gets the address check,
// the daily caps and a known meeting type, and its notes stay off the calendar invite (kept in
// the meeting's metadata). A person in the portal books as before.
func TestServiceBookingGuard(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testMeetingsPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Book " + tid.String()).SetSlug("book-" + tid.String()).ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.ScheduledMeeting.Delete().Where(scheduledmeeting.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})

	guard := NewBookingGuard(&fakeQuota{}, BookingCaps{PerTenantDay: 100, PerEmailDay: 1, PerIPDay: 100}, nil)
	h := NewHandler(NewService(db, nil, zap.NewNop()), zap.NewNop()).WithBookingGuard(guard)
	r := chi.NewRouter()
	r.Post("/meetings", h.Create)
	service := &authclient.Claims{IsService: true, ServiceName: "marketflow-ai", TenantID: tid.String()}
	person := &authclient.Claims{TenantID: tid.String()}
	at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Minute).Format(time.RFC3339)

	post := func(body string, c *authclient.Claims) int {
		req := httptest.NewRequest(http.MethodPost, "/meetings", strings.NewReader(body))
		req.Header.Set("X-Tenant-ID", tid.String())
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(authclient.ContextWithClaims(req.Context(), c))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(`{"scheduled_at":"`+at+`","attendee_email":"Jane <jane@example.com>"}`, service); code != http.StatusBadRequest {
		t.Fatalf("invalid address: %d", code)
	}
	body := `{"scheduled_at":"` + at + `","attendee_email":"Jane@Example.com","notes":"Claim your prize at http://x","meeting_type":"Win a phone"}`
	if code := post(body, service); code != http.StatusCreated {
		t.Fatalf("first booking: %d", code)
	}
	m := db.ScheduledMeeting.Query().Where(scheduledmeeting.TenantID(tid)).OnlyX(ctx)
	if m.Notes != nil {
		t.Errorf("visitor notes reached the invite: %q", *m.Notes)
	}
	if m.Metadata["booking_notes"] != "Claim your prize at http://x" {
		t.Errorf("notes not kept on the record: %v", m.Metadata)
	}
	if m.AttendeeEmail == nil || *m.AttendeeEmail != "jane@example.com" || m.MeetingType != "product_demo" {
		t.Errorf("address or type not normalised: %v %s", m.AttendeeEmail, m.MeetingType)
	}
	// A second invite to the same address the same day is past its cap.
	if code := post(body, service); code != http.StatusTooManyRequests {
		t.Fatalf("second invite to one address: %d", code)
	}

	// A person in the portal: no caps, and the notes stay theirs to send.
	pbody := `{"scheduled_at":"` + at + `","attendee_email":"jane@example.com","notes":"Agenda: pricing"}`
	for i := 0; i < 2; i++ {
		if code := post(pbody, person); code != http.StatusCreated {
			t.Fatalf("portal booking %d: %d", i+1, code)
		}
	}
	n, _ := db.ScheduledMeeting.Query().Where(scheduledmeeting.TenantID(tid), scheduledmeeting.NotesEQ("Agenda: pricing")).Count(ctx)
	if n != 2 {
		t.Fatalf("portal notes kept: %d", n)
	}
}
