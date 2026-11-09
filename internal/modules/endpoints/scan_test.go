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
	sc := NewScanner(st, map[string]string{"ordering": srv.URL}, nil)
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
	sc := NewScanner(st, map[string]string{"ordering": srv.URL}, nil)
	if _, err := sc.ScanService(context.Background(), "ordering"); err == nil {
		t.Fatal("an oversized spec must fail")
	}
	if len(st["ordering"]) != 1 {
		t.Fatal("a failed scan must leave the stored list alone")
	}
}
