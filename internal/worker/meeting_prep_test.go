package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// capture records every request body a fake server receives.
type capture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *capture) server(t *testing.T, status int, reply string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c *capture) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.bodies, "\n")
}

// emails decodes the captured requests as notifications-api reads them
// (to[], template, data), so a wrong wire shape fails the test.
func (c *capture) emails(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, 0, len(c.bodies))
	for _, b := range c.bodies {
		var m map[string]any
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			t.Fatalf("notification body is not JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func seedMeeting(t *testing.T, db *ent.Client, tid uuid.UUID, at time.Time, contactID *uuid.UUID) *ent.ScheduledMeeting {
	t.Helper()
	m, err := db.ScheduledMeeting.Create().SetTenantID(tid).SetScheduledAt(at).SetDurationMinutes(30).
		SetStatus("scheduled").SetAttendeeName("Akinyi").SetOrganizerEmail("owner@example.com").
		SetMeetingURL("https://meet.example.com/abc").SetNillableContactID(contactID).
		SetMetadata(map[string]any{}).Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ScheduledMeeting.Delete().Where(scheduledmeeting.TenantID(tid)).Exec(context.Background()) })
	return m
}

func seedContact(t *testing.T, db *ent.Client, tid uuid.UUID) uuid.UUID {
	t.Helper()
	c, err := db.Contact.Create().SetTenantID(tid).SetContactType("customer").SetFirstName("Akinyi").Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Contact.Delete().Where(contact.TenantID(tid)).Exec(context.Background()) })
	return c.ID
}

func newPrepAgent(db *ent.Client, notifURL, aiURL string, now time.Time) *MeetingPrepAgent {
	a := NewMeetingPrepAgent(db, notifications.New(notifications.Config{BaseURL: notifURL, APIKey: "k"}), aiURL, "svc", "", zap.NewNop())
	a.nowFn = func() time.Time { return now }
	claimed := map[string]bool{}
	a.claim = func(_ context.Context, key string, _ time.Duration) bool {
		if claimed[key] {
			return false
		}
		claimed[key] = true
		return true
	}
	return a
}

func TestMeetingPrepSendsOnceWithTalkingPoints(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "prep")
	now := time.Now().UTC()
	cid := seedContact(t, db, tid)
	due := seedMeeting(t, db, tid, now.Add(30*time.Minute), &cid)
	seedMeeting(t, db, tid, now.Add(3*time.Hour), &cid) // not due yet

	notif, ai := &capture{}, &capture{}
	ns := notif.server(t, http.StatusAccepted, `{"status":"queued"}`)
	as := ai.server(t, http.StatusOK, `{"bio_summary":"Runs a salon in Kisumu.","talking_points":["Ask about the gift box order"]}`)
	a := newPrepAgent(db, ns.URL, as.URL, now)

	sent, err := a.scan(ctx)
	if err != nil || sent != 1 {
		t.Fatalf("scan = %d %v, want 1 brief", sent, err)
	}
	// marketflow-ai's /internal/os/meeting-prep decodes tenant_id and contact_id.
	if req := ai.all(); !strings.Contains(req, `"contact_id":"`+cid.String()+`"`) || !strings.Contains(req, `"tenant_id":"`+tid.String()+`"`) {
		t.Fatalf("AI request = %s", req)
	}
	emails := notif.emails(t)
	if len(emails) != 1 || firstTo(emails[0]) != "owner@example.com" {
		t.Fatalf("want one email to the organizer, got %+v", emails)
	}
	body := dataBody(emails[0])
	for _, want := range []string{"Akinyi", "meet.example.com/abc", "Ask about the gift box order", "Runs a salon in Kisumu."} {
		if !strings.Contains(body, want) {
			t.Fatalf("brief is missing %q: %s", want, body)
		}
	}
	got, _ := db.ScheduledMeeting.Get(ctx, due.ID)
	if v, _ := got.Metadata["prep_sent"].(bool); !v {
		t.Fatal("meeting not marked prep_sent")
	}
	// The next tick sees the same meeting and sends nothing.
	if sent, _ := a.scan(ctx); sent != 0 || notif.count() != 1 {
		t.Fatalf("second scan sent again (sent=%d, emails=%d)", sent, notif.count())
	}
}

func TestMeetingPrepGoesOutWithoutAI(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "prep-noai")
	now := time.Now().UTC()
	cid := seedContact(t, db, tid)
	seedMeeting(t, db, tid, now.Add(28*time.Minute), &cid)

	notif, ai := &capture{}, &capture{}
	ns := notif.server(t, http.StatusAccepted, `{"status":"queued"}`)
	as := ai.server(t, http.StatusBadGateway, `{"error":"meeting prep failed"}`)
	a := newPrepAgent(db, ns.URL, as.URL, now)

	if sent, err := a.scan(ctx); err != nil || sent != 1 {
		t.Fatalf("scan = %d %v; the brief must go out without talking points", sent, err)
	}
	if ai.count() != 1 {
		t.Fatal("marketflow-ai was not asked")
	}
	emails := notif.emails(t)
	if len(emails) != 1 {
		t.Fatalf("want one email, got %d", len(emails))
	}
	if body := dataBody(emails[0]); strings.Contains(body, "Talking points") || !strings.Contains(body, "Akinyi") {
		t.Fatalf("brief without AI = %s", body)
	}
}
