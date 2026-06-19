package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Config holds configuration for the subscriptions S2S client.
type Config struct {
	ServiceURL     string
	APIKey         string
	RequestTimeout time.Duration
}

// Client interacts with subscriptions-api over S2S (internal-service-key auth).
// It is used by NATS event consumers — which carry a tenant_id but no user JWT —
// to gate optional, feature-specific cross-service data projections.
type Client struct {
	cfg  Config
	http *http.Client
}

// NewClient constructs a subscriptions S2S client. A zero RequestTimeout defaults to 5s.
func NewClient(cfg Config) *Client {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.RequestTimeout}}
}

// Entitlements is the tenant subscription snapshot returned by
// GET /api/v1/tenants/{id}/subscription. Only the fields needed for consumer
// gating are modelled; the endpoint returns these at the top level.
type Entitlements struct {
	Features     []string `json:"features"`
	Status       string   `json:"status"`
	BillingMode  string   `json:"billing_mode"`
	IsDemoBypass bool     `json:"is_demo_bypass"`
}

// GetEntitlements fetches the tenant's subscription snapshot from the S2S endpoint.
// Returns nil on any error so callers can fail open.
func (c *Client) GetEntitlements(ctx context.Context, tenantID string) *Entitlements {
	if c == nil || c.cfg.ServiceURL == "" || tenantID == "" {
		return nil
	}
	url := fmt.Sprintf("%s/api/v1/tenants/%s/subscription", c.cfg.ServiceURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var e Entitlements
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return nil
	}
	return &e
}

// entitlementCacheTTL bounds how long a tenant's entitlement snapshot is reused by
// event consumers. 60s absorbs event bursts without hammering subscriptions-api,
// while staying fresh enough that a plan change takes effect quickly.
const entitlementCacheTTL = 60 * time.Second

type cachedEntitlements struct {
	ent     *Entitlements
	fetched time.Time
}

var (
	entCacheMu sync.Mutex
	entCache   = map[string]cachedEntitlements{}
)

// ConsumerHasFeature reports whether a tenant is entitled to featureCode, for use by
// NATS event consumers that have a tenant_id but no user JWT. It mirrors the HTTP-layer
// gating contract:
//
//   - Demo-bypass and service-charge (PAYG) tenants are always allowed.
//   - Otherwise the feature must be present in the tenant's entitlement snapshot.
//
// It FAILS OPEN (returns true) when subscriptions-api is unreachable or the tenant has
// no snapshot, so a subscriptions-api outage never silently drops legitimate data sync.
// Results are cached per tenant for entitlementCacheTTL.
func (c *Client) ConsumerHasFeature(ctx context.Context, tenantID, featureCode string) bool {
	if c == nil || tenantID == "" {
		return true // not wired → fail open
	}
	e := c.cachedEntitlements(ctx, tenantID)
	if e == nil {
		return true // lookup failed → fail open
	}
	if e.IsDemoBypass || e.BillingMode == "service_charge" {
		return true // exempt — mirror IsGatingExempt
	}
	for _, f := range e.Features {
		if f == featureCode {
			return true
		}
	}
	return false
}

func (c *Client) cachedEntitlements(ctx context.Context, tenantID string) *Entitlements {
	entCacheMu.Lock()
	if hit, ok := entCache[tenantID]; ok && time.Since(hit.fetched) < entitlementCacheTTL {
		entCacheMu.Unlock()
		return hit.ent
	}
	entCacheMu.Unlock()

	e := c.GetEntitlements(ctx, tenantID)
	if e == nil {
		return nil // do not cache failures — retry next event
	}
	entCacheMu.Lock()
	entCache[tenantID] = cachedEntitlements{ent: e, fetched: time.Now()}
	entCacheMu.Unlock()
	return e
}
