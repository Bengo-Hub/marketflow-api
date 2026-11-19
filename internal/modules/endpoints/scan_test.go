package endpoints

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memStore map[string][]Endpoint

func (m memStore) Load(_ context.Context, svc string) ([]Endpoint, error) { return m[svc], nil }
func (m memStore) Update(_ context.Context, svc string, fn func([]Endpoint) ([]Endpoint, error)) error {
	next, err := fn(m[svc])
	if err != nil {
		return err
	}
	m[svc] = next
	return nil
}

func TestScanServiceStoresCandidatesAndKeepsDecisions(t *testing.T) {
	spec := `{"swagger":"2.0","basePath":"/api/v1","paths":{
		"/{tenant}/promotions/deals":{"get":{"tags":["Promotions"]}},
		"/{tenant}/catalog/items/by-category":{"get":{"tags":["Catalog"]}},
		"/pay/{tenant}/gateways":{"get":{"tags":["Payments"]}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != SpecPath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(spec))
	}))
	defer srv.Close()

	st := memStore{"ordering": {{Method: "GET", Path: "/api/v1/{tenant}/promotions/deals", Status: StatusApproved}}}
	sc := NewScanner(st, map[string]Service{"ordering": {Base: srv.URL}}, "", nil)
	sc.now = func() time.Time { return time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC) }

	added, err := sc.ScanService(context.Background(), "ordering")
	if err != nil || added != 1 {
		t.Fatalf("added %d, %v", added, err)
	}
	got := st["ordering"]
	if len(got) != 2 || got[1].Status != StatusApproved || got[0].Status != StatusCandidate {
		t.Fatalf("stored %+v", got)
	}

	if _, err := sc.ScanService(context.Background(), "nope"); err == nil {
		t.Fatal("an unknown service must fail")
	}
}

func TestScanServiceRefusesAnOversizedOrBrokenSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", maxSpecBytes+10)))
	}))
	defer srv.Close()
	st := memStore{"ordering": {{Method: "GET", Path: "/keep", Status: StatusApproved}}}
	sc := NewScanner(st, map[string]Service{"ordering": {Base: srv.URL}}, "", nil)
	if _, err := sc.ScanService(context.Background(), "ordering"); err == nil {
		t.Fatal("an oversized spec must fail")
	}
	if len(st["ordering"]) != 1 {
		t.Fatal("a failed scan must leave the stored list alone")
	}
}

// statusStore is a memStore that also keeps scan statuses.
type statusStore struct {
	memStore
	status map[string]ScanStatus
}

func (s *statusStore) SaveStatus(_ context.Context, svc string, st ScanStatus) error {
	s.status[svc] = st
	return nil
}

// notifications and treasury serve their full spec only to a trusted key and answer everyone
// else with a public subset (X-Docs-View: external). The scan sends its key, reads the configured
// spec path and records what it got.
func TestScanSendsTheDocsKeyAndRecordsTheView(t *testing.T) {
	full := `{"swagger":"2.0","basePath":"/api/v1","paths":{
		"/analytics/delivery/{tenantId}":{"get":{"tags":["Analytics"]}},
		"/analytics/delivery":{"get":{"tags":["Analytics"]}},
		"/notifications/send":{"post":{"tags":["Notifications"]}}}}`
	public := `{"swagger":"2.0","basePath":"/api/v1","paths":{"/notifications/send":{"post":{"tags":["Notifications"]}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/docs/spec.json" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") == "scan-key" {
			w.Header().Set("X-Docs-View", "internal")
			_, _ = w.Write([]byte(full))
			return
		}
		w.Header().Set("X-Docs-View", "external")
		_, _ = w.Write([]byte(public))
	}))
	defer srv.Close()
	svc := map[string]Service{"notifications": {Base: srv.URL, SpecPath: "/docs/spec.json"}}

	st := &statusStore{memStore: memStore{}, status: map[string]ScanStatus{}}
	if _, err := NewScanner(st, svc, "", nil).ScanService(context.Background(), "notifications"); err != nil {
		t.Fatal(err)
	}
	if got := st.status["notifications"]; got.DocsView != "external" || got.Operations != 1 || got.Offerable != 0 {
		t.Fatalf("keyless scan status = %+v", got)
	}

	added, err := NewScanner(st, svc, "scan-key", nil).ScanService(context.Background(), "notifications")
	if err != nil || added != 1 {
		t.Fatalf("added %d, %v", added, err)
	}
	got := st.status["notifications"]
	if got.DocsView != "internal" || got.Operations != 3 || got.Offerable != 1 || got.Added != 1 || got.Error != "" {
		t.Fatalf("keyed scan status = %+v", got)
	}
	if list := st.memStore["notifications"]; len(list) != 1 || list[0].Path != "/api/v1/analytics/delivery/{tenantId}" {
		t.Fatalf("only the tenant-scoped analytics read is offered: %+v", list)
	}

	if _, err := NewScanner(st, map[string]Service{"notifications": {Base: srv.URL}}, "scan-key", nil).ScanService(context.Background(), "notifications"); err == nil {
		t.Fatal("a missing spec must fail")
	}
	if st.status["notifications"].Error == "" {
		t.Fatal("a failed scan records its error")
	}
}
