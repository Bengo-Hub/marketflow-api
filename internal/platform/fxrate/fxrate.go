// Package fxrate reads exchange rates from treasury-api, the platform's one source of rates
// (fetched daily from the configured forex provider and kept there). marketflow-ai's
// internal/fxrate reads the same treasury route for its credit engine; MarketFlow never keeps a
// rate of its own and never hard-codes one.
package fxrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
)

// cacheFor is how long a fetched rate is reused (in the shared Redis cache, so every pod uses the
// same rate); treasury updates its rates daily.
const cacheFor = 6 * time.Hour

// probeAmount is the amount converted to read a rate. Treasury rounds the result at the target
// currency's places, so converting 1 UGX to KES would read 0.03; a million keeps the precision.
const probeAmount = 1_000_000

// Sanity bounds. A KES per USD outside 50 to 500, or any rate outside these wide bounds, is a
// misconfiguration, never a real rate.
const (
	minKESPerUSD, maxKESPerUSD   = 50, 500
	minKESPerUnit, maxKESPerUnit = 0.0001, 100_000
)

// ErrNoRate means no usable rate could be read. Callers that gate money on a rate fail safe.
var ErrNoRate = errors.New("fxrate: no exchange rate")

// Rates reads rates from treasury-api over S2S.
type Rates struct {
	base, key, tenant string
	cache             *sharedcache.Aside
	http              *http.Client
}

// New reads platform-level rates of platformTenant (the platform owner's slug) from treasuryURL
// with the internal service key. cache may be nil (no caching).
func New(treasuryURL, serviceKey, platformTenant string, cache *sharedcache.Aside) *Rates {
	return &Rates{base: strings.TrimRight(strings.TrimSpace(treasuryURL), "/"), key: serviceKey,
		tenant: strings.TrimSpace(platformTenant), cache: cache, http: &http.Client{Timeout: 5 * time.Second}}
}

// KESPer is how many KES one unit of currency is worth: 1 for KES, else the cached treasury rate,
// else a fresh one. Any failure (treasury down, no rate for that currency, an implausible rate)
// returns an error wrapping ErrNoRate; a failure is never cached.
func (r *Rates) KESPer(ctx context.Context, currency string) (float64, error) {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if cur == "KES" {
		return 1, nil
	}
	if !isCode(cur) {
		return 0, fmt.Errorf("%w: currency %q", ErrNoRate, currency)
	}
	if r == nil || r.base == "" || r.key == "" || r.tenant == "" {
		return 0, fmt.Errorf("%w: treasury is not configured", ErrNoRate)
	}
	return sharedcache.GetOrSet(ctx, r.cache, sharedcache.Key("marketflow", "fx", cur, "KES"), cacheFor, func(ctx context.Context) (float64, error) {
		return r.fetch(ctx, cur)
	})
}

// isCode accepts an ISO 4217 style code: three letters A to Z.
func isCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

func (r *Rates) fetch(ctx context.Context, cur string) (float64, error) {
	q := url.Values{"from": {cur}, "to": {"KES"}, "amount": {fmt.Sprint(probeAmount)}}
	u := fmt.Sprintf("%s/api/v1/s2s/%s/exchange-rates/convert?%s", r.base, url.PathEscape(r.tenant), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrNoRate, err)
	}
	req.Header.Set("X-API-Key", r.key)
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: treasury convert: %v", ErrNoRate, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: treasury convert %s: status %d", ErrNoRate, cur, resp.StatusCode)
	}
	var out struct {
		Converted json.Number `json:"converted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("%w: treasury convert %s: %v", ErrNoRate, cur, err)
	}
	converted, err := out.Converted.Float64()
	if err != nil {
		return 0, fmt.Errorf("%w: treasury convert %s: %v", ErrNoRate, cur, err)
	}
	rate := converted / probeAmount
	if err := plausible(cur, converted, rate); err != nil {
		return 0, err
	}
	return rate, nil
}

// plausible rejects what cannot be a real rate. An older treasury answered a pair it had no rate
// for with the amount unchanged, so a converted amount equal to the probe is no rate.
func plausible(cur string, converted, rate float64) error {
	switch {
	case math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0:
		return fmt.Errorf("%w: treasury convert %s: rate %v", ErrNoRate, cur, rate)
	case converted == probeAmount:
		return fmt.Errorf("%w: treasury has no %s rate (amount returned unchanged)", ErrNoRate, cur)
	case cur == "USD" && (rate < minKESPerUSD || rate > maxKESPerUSD):
		return fmt.Errorf("%w: implausible KES per USD %v", ErrNoRate, rate)
	case rate < minKESPerUnit || rate > maxKESPerUnit:
		return fmt.Errorf("%w: implausible KES per %s %v", ErrNoRate, cur, rate)
	}
	return nil
}
