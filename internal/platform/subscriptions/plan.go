package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// PlanSnapshot is what a plan grants: its included feature codes and numeric limits.
type PlanSnapshot struct {
	Code     string
	Features []string
	Limits   map[string]int
}

// planCacheTTL: plans change only when the seed runs, so an hour is plenty.
const planCacheTTL = time.Hour

var (
	planCacheMu sync.Mutex
	planCache   = map[string]planCacheEntry{}
)

type planCacheEntry struct {
	plan    *PlanSnapshot
	fetched time.Time
}

// GetPlan reads a plan by code (GET /api/v1/plans/code/{code}, public), cached for an hour.
// nil on any error, and failures are not cached. Used to give the demo tenant exactly the free
// plan's features and limits (owner, 2026-11-12).
func (c *Client) GetPlan(ctx context.Context, code string) *PlanSnapshot {
	if c == nil || c.cfg.ServiceURL == "" || code == "" {
		return nil
	}
	planCacheMu.Lock()
	if hit, ok := planCache[code]; ok && time.Since(hit.fetched) < planCacheTTL {
		planCacheMu.Unlock()
		return hit.plan
	}
	planCacheMu.Unlock()

	p, err := c.fetchPlan(ctx, code)
	if err != nil {
		return nil
	}
	planCacheMu.Lock()
	planCache[code] = planCacheEntry{plan: p, fetched: time.Now()}
	planCacheMu.Unlock()
	return p
}

func (c *Client) fetchPlan(ctx context.Context, code string) (*PlanSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/plans/code/%s", c.cfg.ServiceURL, url.PathEscape(code)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plan %s: status %d", code, resp.StatusCode)
	}
	var body struct {
		Plan struct {
			PlanCode   string         `json:"planCode"`
			TierLimits map[string]any `json:"tierLimits"`
			Features   []struct {
				FeatureCode string `json:"featureCode"`
				IsIncluded  bool   `json:"isIncluded"`
			} `json:"features"`
		} `json:"plan"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	out := &PlanSnapshot{Code: body.Plan.PlanCode, Limits: map[string]int{}}
	for _, f := range body.Plan.Features {
		if f.IsIncluded && f.FeatureCode != "" {
			out.Features = append(out.Features, f.FeatureCode)
		}
	}
	for k, v := range body.Plan.TierLimits {
		if n, ok := v.(float64); ok {
			out.Limits[k] = int(n)
		}
	}
	return out, nil
}
