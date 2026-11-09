package endpoints

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// SpecPath is where every Codevertex Go service serves its OpenAPI JSON.
const SpecPath = "/api/v1/openapi.json"

// maxSpecBytes bounds one spec read; the largest today is about 300 KB.
const maxSpecBytes = 5 << 20

// DefaultServices are the in-cluster base URLs of the services scanned each week (the same hosts
// marketflow-ai's query_service_data calls). marketflow itself is left out: Vera reads its own
// data through dedicated tools.
var DefaultServices = map[string]string{
	"ordering":      "http://ordering-backend.ordering.svc.cluster.local:4000",
	"inventory":     "http://inventory-api.inventory.svc.cluster.local:4000",
	"treasury":      "http://treasury-api.treasury.svc.cluster.local:4000",
	"subscriptions": "http://subscription-api.subscriptions.svc.cluster.local:4000",
	"pos":           "http://pos-api.pos.svc.cluster.local:4000",
	"logistics":     "http://logistics-api.logistics.svc.cluster.local:4000",
	"notifications": "http://notifications-api.notifications.svc.cluster.local:4000",
}

// Store keeps each service's endpoint list. Update applies fn to the current list and writes the
// result without losing a concurrent writer's change.
type Store interface {
	Load(ctx context.Context, service string) ([]Endpoint, error)
	Update(ctx context.Context, service string, fn func([]Endpoint) ([]Endpoint, error)) error
}

// Scanner reads each service's spec and folds new routes into the store as candidates.
type Scanner struct {
	store  Store
	bases  map[string]string
	http   *http.Client
	logger *zap.Logger
	now    func() time.Time
}

func NewScanner(store Store, bases map[string]string, logger *zap.Logger) *Scanner {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Scanner{store: store, bases: bases, http: &http.Client{Timeout: 20 * time.Second}, logger: logger.Named("endpoint-scan"), now: time.Now}
}

// Bases returns the scanned services and their base URLs.
func (s *Scanner) Bases() map[string]string { return s.bases }

// ScanService scans one service and returns how many new candidates it stored. A failed read
// leaves the stored list untouched.
func (s *Scanner) ScanService(ctx context.Context, service string) (int, error) {
	base, ok := s.bases[service]
	if !ok {
		return 0, fmt.Errorf("unknown service %q", service)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+SpecPath, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s spec: %w", service, err)
	}
	defer resp.Body.Close()
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
	added := 0
	err = s.store.Update(ctx, service, func(existing []Endpoint) ([]Endpoint, error) {
		merged, n := Merge(existing, ops, s.now().UTC())
		added = n
		return merged, nil
	})
	if err != nil {
		return 0, err
	}
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
	names := make([]string, 0, len(s.bases))
	for n := range s.bases {
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
