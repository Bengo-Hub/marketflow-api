package endpoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// SpecPath is where every Codevertex Go service serves its OpenAPI JSON, unless a service's
// config names another path.
const SpecPath = "/api/v1/openapi.json"

// maxSpecBytes bounds one spec read; the largest today is about 350 KB.
const maxSpecBytes = 5 << 20

// Service is one scanned service: its in-cluster base URL, the path of its spec (SpecPath when
// empty) and the paths it may offer without a tenant param (platform-wide catalogues).
type Service struct {
	Base        string   `json:"base"`
	SpecPath    string   `json:"spec_path,omitempty"`
	PublicPaths []string `json:"public_paths,omitempty"`
}

func (s Service) specURL() string {
	p := s.SpecPath
	if p == "" {
		p = SpecPath
	}
	return strings.TrimSuffix(s.Base, "/") + "/" + strings.TrimPrefix(p, "/")
}

// DefaultServices are the services scanned each week (the same hosts marketflow-ai's
// query_service_data calls). marketflow itself is left out: Vera reads its own data through
// dedicated tools. MF_ENDPOINT_SCAN_SERVICES changes or adds entries (see ServicesFromConfig).
func DefaultServices() map[string]Service {
	return map[string]Service{
		"ordering":      {Base: "http://ordering-backend.ordering.svc.cluster.local:4000"},
		"inventory":     {Base: "http://inventory-api.inventory.svc.cluster.local:4000"},
		"treasury":      {Base: "http://treasury-api.treasury.svc.cluster.local:4000"},
		"subscriptions": {Base: "http://subscription-api.subscriptions.svc.cluster.local:4000", PublicPaths: []string{"/api/v1/plans", "/api/v1/features"}},
		"pos":           {Base: "http://pos-api.pos.svc.cluster.local:4000"},
		"logistics":     {Base: "http://logistics-api.logistics.svc.cluster.local:4000"},
		"notifications": {Base: "http://notifications-api.notifications.svc.cluster.local:4000"},
	}
}

// ServicesFromConfig merges a JSON object of name to Service over the defaults. A field left out
// keeps the default's value, and an entry with "base": "" removes the service.
func ServicesFromConfig(raw string) (map[string]Service, error) {
	out := DefaultServices()
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	var over map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &over); err != nil {
		return nil, fmt.Errorf("endpoint scan services: %w", err)
	}
	for name, msg := range over {
		s := out[name]
		if err := json.Unmarshal(msg, &s); err != nil {
			return nil, fmt.Errorf("endpoint scan service %s: %w", name, err)
		}
		if s.Base == "" {
			delete(out, name)
			continue
		}
		out[name] = s
	}
	return out, nil
}

// Store keeps each service's endpoint list. Update applies fn to the current list and writes the
// result without losing a concurrent writer's change.
type Store interface {
	Load(ctx context.Context, service string) ([]Endpoint, error)
	Update(ctx context.Context, service string, fn func([]Endpoint) ([]Endpoint, error)) error
}

// StatusStore keeps the last scan of each service, so the owner's page can say why a service
// offers nothing. A Store without it simply records no status.
type StatusStore interface {
	SaveStatus(ctx context.Context, service string, st ScanStatus) error
}

// ScanStatus is the outcome of one service's last scan.
type ScanStatus struct {
	ScannedAt time.Time `json:"scanned_at"`
	Error     string    `json:"error,omitempty"`
	// Operations is how many operations the spec listed; Offerable how many passed the filters.
	Operations int `json:"operations"`
	Offerable  int `json:"offerable"`
	Added      int `json:"added"`
	// DocsView is the X-Docs-View the service answered with: "external" means it served only its
	// public subset, so the scan key was missing or not accepted.
	DocsView string `json:"docs_view,omitempty"`
}

// Scanner reads each service's spec and folds new routes into the store as candidates.
type Scanner struct {
	store    Store
	services map[string]Service
	docsKey  string
	http     *http.Client
	logger   *zap.Logger
	now      func() time.Time
}

// NewScanner builds a scanner. docsKey is sent as X-API-Key: services that serve their full spec
// only to a trusted caller (notifications, inventory) answer with the public subset without it.
func NewScanner(store Store, services map[string]Service, docsKey string, logger *zap.Logger) *Scanner {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Scanner{store: store, services: services, docsKey: docsKey, http: &http.Client{Timeout: 20 * time.Second}, logger: logger.Named("endpoint-scan"), now: time.Now}
}

// Services returns the scanned services.
func (s *Scanner) Services() map[string]Service { return s.services }

// ScanService scans one service and returns how many new candidates it stored. A failed read
// leaves the stored list untouched; either way the outcome is recorded when the store keeps
// statuses.
func (s *Scanner) ScanService(ctx context.Context, service string) (int, error) {
	svc, ok := s.services[service]
	if !ok {
		return 0, fmt.Errorf("unknown service %q", service)
	}
	st := ScanStatus{ScannedAt: s.now().UTC()}
	added, err := s.scan(ctx, service, svc, &st)
	if err != nil {
		st.Error = err.Error()
	}
	if ss, ok := s.store.(StatusStore); ok {
		if serr := ss.SaveStatus(ctx, service, st); serr != nil {
			s.logger.Warn("endpoint scan status not saved", zap.String("service", service), zap.Error(serr))
		}
	}
	if st.DocsView == "external" {
		s.logger.Warn("service served only its public spec; set MF_ENDPOINT_SCAN_DOCS_KEY to a key it trusts", zap.String("service", service))
	}
	return added, err
}

func (s *Scanner) scan(ctx context.Context, service string, svc Service, st *ScanStatus) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.specURL(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if s.docsKey != "" {
		req.Header.Set("X-API-Key", s.docsKey)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s spec: %w", service, err)
	}
	defer resp.Body.Close()
	st.DocsView = resp.Header.Get("X-Docs-View")
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s spec: status %d", service, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
	if err != nil {
		return 0, fmt.Errorf("%s spec: %w", service, err)
	}
	if len(body) > maxSpecBytes {
		return 0, fmt.Errorf("%s spec is larger than %d bytes", service, maxSpecBytes)
	}
	ops, err := ParseSpec(body)
	if err != nil {
		return 0, fmt.Errorf("%s spec: %w", service, err)
	}
	st.Operations = len(ops)
	added := 0
	err = s.store.Update(ctx, service, func(existing []Endpoint) ([]Endpoint, error) {
		merged, n := Merge(existing, ops, s.now().UTC(), svc.PublicPaths)
		added, st.Offerable = n, len(merged)
		return merged, nil
	})
	if err != nil {
		return 0, err
	}
	st.Added = added
	return added, nil
}

// RunWeekly checks every 6 hours and scans once per ISO week. claim (sharedcache.ClaimOnce in
// production) makes sure a restart or a second worker pod never scans the same week twice.
func (s *Scanner) RunWeekly(ctx context.Context, claim func(ctx context.Context, key string, ttl time.Duration) bool) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		year, week := s.now().UTC().ISOWeek()
		if claim(ctx, fmt.Sprintf("marketflow:endpoint-scan:%d-%02d", year, week), 8*24*time.Hour) {
			s.ScanAll(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ScanAll scans every service; one service failing does not stop the rest.
func (s *Scanner) ScanAll(ctx context.Context) (added int, failed []string) {
	names := make([]string, 0, len(s.services))
	for n := range s.services {
		names = append(names, n)
	}
	sort.Strings(names)
	var errs []error
	for _, n := range names {
		a, err := s.ScanService(ctx, n)
		if err != nil {
			failed = append(failed, n)
			errs = append(errs, err)
			continue
		}
		added += a
	}
	s.logger.Info("endpoint scan finished", zap.Int("new_candidates", added), zap.Strings("failed", failed), zap.Error(errors.Join(errs...)))
	return added, failed
}
