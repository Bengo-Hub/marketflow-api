package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/modules/contacts"
)

// fakePartyContacts records what the handler asked of the contacts service.
type fakePartyContacts struct {
	mu        sync.Mutex
	exists    bool
	deleteErr error
	deleted   []uuid.UUID
	erased    []contacts.ErasureRecord
	tenants   []uuid.UUID
	failures  []string
}

func (f *fakePartyContacts) Exists(_ context.Context, tenantID, _ uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants = append(f.tenants, tenantID)
	return f.exists, nil
}

func (f *fakePartyContacts) DeleteErased(_ context.Context, _, contactID uuid.UUID, _ contacts.ErasureRecord) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	f.deleted = append(f.deleted, contactID)
	return true, nil
}

func (f *fakePartyContacts) RecordErasureFailure(_ context.Context, tenantID, contactID uuid.UUID, rec contacts.ErasureRecord, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, fmt.Sprintf("%s/%s/%s/%s", tenantID, contactID, rec.SourceID, reason))
	return nil
}

func (f *fakePartyContacts) Erase(_ context.Context, _, _ uuid.UUID, rec contacts.ErasureRecord) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.erased = append(f.erased, rec)
	return true, nil
}

// linkServer answers the crm-links check the way a service would; it records each request.
type linkServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []*http.Request
}

func newLinkServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *linkServer {
	t.Helper()
	ls := &linkServer{}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		ls.hits = append(ls.hits, r)
		ls.mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(ls.srv.Close)
	return ls
}

func answer(referenced bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"referenced":%t}`, referenced)
	}
}

func partyEvent(t *testing.T, tenantID, contactID uuid.UUID) []byte {
	t.Helper()
	party := uuid.New()
	ev := eventslib.NewEvent("privacy.party_anonymised", "maskani", party, tenantID, map[string]any{
		"party_id": party.String(), "crm_contact_id": contactID.String(), "tenant_id": tenantID.String()})
	b, err := ev.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newPartyHandler(fc *fakePartyContacts, services []linkService, timeout time.Duration) *PartyAnonymisedHandler {
	return &PartyAnonymisedHandler{
		contacts: fc,
		links:    &crmLinkChecker{services: services, key: "svc-key", hc: newLinkHTTPClient(), timeout: timeout},
		logger:   zap.NewNop(),
	}
}

func TestPartyAnonymisedSubjectAndStream(t *testing.T) {
	h := &PartyAnonymisedHandler{}
	if h.Subject() != "maskani.privacy.party_anonymised" {
		t.Fatalf("subject = %q", h.Subject())
	}
	r := &Registry{cfg: &Config{NATS: NATSConfig{Stream: "marketflow"}}}
	if got := r.streamFor(h.Subject()); got != "maskani" {
		t.Fatalf("stream = %q, want maskani", got)
	}
}

// Every service answers "not referenced": the contact is deleted through the normal path, and
// each service was asked for this tenant and contact with the service key.
func TestPartyAnonymisedDeletesWhenNoServiceLinks(t *testing.T) {
	tid, cid := uuid.New(), uuid.New()
	a, b := newLinkServer(t, answer(false)), newLinkServer(t, answer(false))
	fc := &fakePartyContacts{exists: true}
	h := newPartyHandler(fc, []linkService{{"pos", a.srv.URL}, {"treasury", b.srv.URL + "/"}}, time.Second)

	if err := h.Handle(context.Background(), fakeMsg{data: partyEvent(t, tid, cid)}); err != nil {
		t.Fatal(err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != cid || len(fc.erased) != 0 {
		t.Fatalf("deleted=%v erased=%v", fc.deleted, fc.erased)
	}
	for _, s := range []*linkServer{a, b} {
		if len(s.hits) != 1 {
			t.Fatalf("hits = %d", len(s.hits))
		}
		r := s.hits[0]
		if want := "/api/v1/s2s/" + tid.String() + "/crm-links/" + cid.String(); r.URL.Path != want {
			t.Fatalf("path = %q, want %q", r.URL.Path, want)
		}
		if r.Header.Get("X-API-Key") != "svc-key" {
			t.Fatal("missing service key")
		}
	}
	if len(fc.tenants) != 1 || fc.tenants[0] != tid {
		t.Fatalf("tenant scope = %v", fc.tenants)
	}
}

// One service still links the contact: it is stripped, never deleted, and the reason names it.
func TestPartyAnonymisedStripsWhenReferenced(t *testing.T) {
	tid, cid := uuid.New(), uuid.New()
	a, b := newLinkServer(t, answer(false)), newLinkServer(t, answer(true))
	fc := &fakePartyContacts{exists: true}
	h := newPartyHandler(fc, []linkService{{"pos", a.srv.URL}, {"library", b.srv.URL}}, time.Second)

	if err := h.Handle(context.Background(), fakeMsg{data: partyEvent(t, tid, cid)}); err != nil {
		t.Fatal(err)
	}
	if len(fc.deleted) != 0 || len(fc.erased) != 1 {
		t.Fatalf("deleted=%v erased=%v", fc.deleted, fc.erased)
	}
	rec := fc.erased[0]
	if rec.Source != "privacy.party_anonymised" || rec.SourceID == "" {
		t.Fatalf("record = %+v", rec)
	}
	if strings.Join(rec.Reasons, ";") != "library: referenced" {
		t.Fatalf("reasons = %v", rec.Reasons)
	}
}

// A service that does not answer in time, has no endpoint (404) or answers garbage cannot vouch
// for the contact: the safe side is to strip.
func TestPartyAnonymisedStripsWhenAServiceCannotAnswer(t *testing.T) {
	tid, cid := uuid.New(), uuid.New()
	slow := newLinkServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		answer(false)(w, r)
	})
	missing := newLinkServer(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	garbled := newLinkServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	ok := newLinkServer(t, answer(false))
	fc := &fakePartyContacts{exists: true}
	h := newPartyHandler(fc, []linkService{
		{"hospital", slow.srv.URL}, {"ordering", missing.srv.URL}, {"pos", garbled.srv.URL}, {"treasury", ok.srv.URL},
	}, 100*time.Millisecond)

	start := time.Now()
	if err := h.Handle(context.Background(), fakeMsg{data: partyEvent(t, tid, cid)}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("the checks must run in parallel under the per-call limit, took %v", time.Since(start))
	}
	if len(fc.deleted) != 0 || len(fc.erased) != 1 {
		t.Fatalf("deleted=%v erased=%v", fc.deleted, fc.erased)
	}
	got := strings.Join(fc.erased[0].Reasons, ";")
	for _, want := range []string{"hospital: no answer", "ordering: http 404", "pos: bad answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reasons %q lack %q", got, want)
		}
	}
	if strings.Contains(got, "treasury") {
		t.Fatalf("a clean answer is not a reason: %q", got)
	}
}

// No service configured means nobody vouched: strip.
func TestPartyAnonymisedStripsWithNoServicesConfigured(t *testing.T) {
	fc := &fakePartyContacts{exists: true}
	h := newPartyHandler(fc, nil, time.Second)
	if err := h.Handle(context.Background(), fakeMsg{data: partyEvent(t, uuid.New(), uuid.New())}); err != nil {
		t.Fatal(err)
	}
	if len(fc.deleted) != 0 || len(fc.erased) != 1 || fc.erased[0].Reasons[0] != "no services configured" {
		t.Fatalf("deleted=%v erased=%v", fc.deleted, fc.erased)
	}
}

// A 200 without the flag ({}) is not a "no": strip.
func TestPartyAnonymisedStripsOnAnswerWithoutFlag(t *testing.T) {
	empty := newLinkServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) })
	fc := &fakePartyContacts{exists: true}
	h := newPartyHandler(fc, []linkService{{"pos", empty.srv.URL}}, time.Second)
	if err := h.Handle(context.Background(), fakeMsg{data: partyEvent(t, uuid.New(), uuid.New())}); err != nil {
		t.Fatal(err)
	}
	if len(fc.deleted) != 0 || len(fc.erased) != 1 || fc.erased[0].Reasons[0] != "pos: bad answer" {
		t.Fatalf("deleted=%v erased=%v", fc.deleted, fc.erased)
	}
}

// A redirect is never followed (the service key would go with it): it is a non-200, so strip.
func TestPartyAnonymisedStripsOnRedirect(t *testing.T) {
	target := newLinkServer(t, answer(false))
	redirect := newLinkServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+r.URL.Path, http.StatusFound)
	})
	fc := &fakePartyContacts{exists: true}
	h := newPartyHandler(fc, []linkService{{"pos", redirect.srv.URL}}, time.Second)
	if err := h.Handle(context.Background(), fakeMsg{data: partyEvent(t, uuid.New(), uuid.New())}); err != nil {
		t.Fatal(err)
	}
	if len(target.hits) != 0 {
		t.Fatal("the redirect was followed")
	}
	if len(fc.deleted) != 0 || len(fc.erased) != 1 || fc.erased[0].Reasons[0] != "pos: http 302" {
		t.Fatalf("deleted=%v erased=%v", fc.deleted, fc.erased)
	}
}

// Given up after the last delivery: the request is kept as an audit row, with ids only.
func TestPartyAnonymisedRecordsFinalFailure(t *testing.T) {
	fc := &fakePartyContacts{exists: true}
	h := newPartyHandler(fc, nil, time.Second)
	var _ finalFailureRecorder = h
	tid, cid := uuid.New(), uuid.New()
	h.OnFinalFailure(context.Background(), fakeMsg{data: partyEvent(t, tid, cid)})
	if len(fc.failures) != 1 || !strings.HasPrefix(fc.failures[0], tid.String()+"/"+cid.String()+"/") ||
		!strings.HasSuffix(fc.failures[0], "/gave up after the last delivery") {
		t.Fatalf("failures = %v", fc.failures)
	}
	// An undecodable event has nothing to record.
	h.OnFinalFailure(context.Background(), fakeMsg{data: []byte("not json")})
	if len(fc.failures) != 1 {
		t.Fatalf("failures = %v", fc.failures)
	}
}

// Any delete failure is retried, not turned into a strip.
func TestPartyAnonymisedRetriesOnDeleteFailure(t *testing.T) {
	a := newLinkServer(t, answer(false))
	fc := &fakePartyContacts{exists: true, deleteErr: errors.New("connection reset")}
	h := newPartyHandler(fc, []linkService{{"pos", a.srv.URL}}, time.Second)
	if err := h.Handle(context.Background(), fakeMsg{data: partyEvent(t, uuid.New(), uuid.New())}); err == nil {
		t.Fatal("want an error so the message is redelivered")
	}
	if len(fc.erased) != 0 {
		t.Fatal("a transient failure must not strip")
	}
}

// Redelivery after the contact is gone: nothing to do, nobody is asked.
func TestPartyAnonymisedMissingContactIsNoop(t *testing.T) {
	a := newLinkServer(t, answer(false))
	fc := &fakePartyContacts{exists: false}
	h := newPartyHandler(fc, []linkService{{"pos", a.srv.URL}}, time.Second)
	if err := h.Handle(context.Background(), fakeMsg{data: partyEvent(t, uuid.New(), uuid.New())}); err != nil {
		t.Fatal(err)
	}
	if len(a.hits) != 0 || len(fc.deleted) != 0 || len(fc.erased) != 0 {
		t.Fatalf("hits=%d deleted=%v erased=%v", len(a.hits), fc.deleted, fc.erased)
	}
}

// termMsg settles a poison message.
type termMsg struct {
	jetstream.Msg
	data   []byte
	termed bool
}

func (m *termMsg) Data() []byte    { return m.data }
func (m *termMsg) Subject() string { return "maskani.privacy.party_anonymised" }
func (m *termMsg) Term() error     { m.termed = true; return nil }

func TestPartyAnonymisedDropsBadEvents(t *testing.T) {
	fc := &fakePartyContacts{exists: true}
	h := newPartyHandler(fc, nil, time.Second)
	bad := map[string]map[string]any{
		"no contact": {"tenant_id": uuid.NewString(), "party_id": uuid.NewString()},
		"bad tenant": {"tenant_id": "x", "crm_contact_id": uuid.NewString()},
	}
	for name, payload := range bad {
		raw, _ := json.Marshal(map[string]any{"event_type": "privacy.party_anonymised", "payload": payload})
		msg := &termMsg{data: raw}
		if err := h.Handle(context.Background(), msg); !errors.Is(err, errPoisonDropped) || !msg.termed {
			t.Fatalf("%s: err=%v termed=%v", name, err, msg.termed)
		}
	}
	if len(fc.deleted)+len(fc.erased) != 0 {
		t.Fatal("a bad event must change nothing")
	}
}

func TestParseLinkServices(t *testing.T) {
	got := parseLinkServices(" pos=http://pos:4000 , bad, treasury=http://t:4000/,=http://x, hospital= ")
	if len(got) != 2 || got[0] != (linkService{"pos", "http://pos:4000"}) || got[1] != (linkService{"treasury", "http://t:4000"}) {
		t.Fatalf("services = %v", got)
	}
	def := parseLinkServices(defaultCRMLinkServices)
	names := map[string]bool{}
	for _, s := range def {
		names[s.Name] = true
		if !strings.Contains(s.BaseURL, ".svc.cluster.local") {
			t.Fatalf("default %s must stay in-cluster: %s", s.Name, s.BaseURL)
		}
	}
	for _, n := range []string{"pos", "treasury", "library", "hospital", "ordering", "ticketing", "projects"} {
		if !names[n] {
			t.Fatalf("default list lacks %s", n)
		}
	}
}
