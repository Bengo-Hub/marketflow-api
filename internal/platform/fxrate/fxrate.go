// Package fxrate is MarketFlow's one helper for exchange rates and conversions. Rates come from
// treasury-api, the platform's one source (fetched daily from the configured forex provider and
// kept there); marketflow-ai's internal/fxrate reads the same treasury route for its credit
// engine. MarketFlow never keeps a rate table of its own and never hard-codes a rate: the only
// fallback below the last treasury rate is the configured default (MF_MONEY_FX_DEFAULT_KES_PER).
//
// Read order: a fresh cached rate (shared Redis, ttl), else treasury, else the last rate treasury
// gave (up to ttl+staleFor old), else, for reports and estimates only, the configured default.
package fxrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/Bengo-Hub/httpware/money"
)

// Defaults for the cache policy when the caller sets none (see config MF_MONEY_FX_*).
const (
	DefaultTTL      = 6 * time.Hour
	DefaultStaleFor = 30 * 24 * time.Hour
)

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

// Source tells where a rate came from.
const (
	SourceTreasury = "treasury"
	SourceDefault  = "default"
)

// Rates reads rates from treasury-api over S2S.
type Rates struct {
	base, key, tenant string
	cache             *sharedcache.Aside
	http              *http.Client
	ttl, staleFor     time.Duration
	defaults          map[string]float64
}

// New reads platform-level rates of platformTenant (the platform owner's slug) from treasuryURL
// with the internal service key. cache may be nil (no caching).
func New(treasuryURL, serviceKey, platformTenant string, cache *sharedcache.Aside) *Rates {
	return &Rates{base: strings.TrimRight(strings.TrimSpace(treasuryURL), "/"), key: serviceKey,
		tenant: strings.TrimSpace(platformTenant), cache: cache, http: &http.Client{Timeout: 5 * time.Second},
		ttl: DefaultTTL, staleFor: DefaultStaleFor}
}

// WithCachePolicy sets how long a rate is fresh and how long after that the last rate is still
// used while treasury cannot answer. Zero keeps the default.
func (r *Rates) WithCachePolicy(ttl, staleFor time.Duration) *Rates {
	if ttl > 0 {
		r.ttl = ttl
	}
	if staleFor > 0 {
		r.staleFor = staleFor
	}
	return r
}

// WithDefaults sets the configured KES per unit used by KESPerOrDefault when treasury has never
// answered. Implausible values are dropped.
func (r *Rates) WithDefaults(kesPer map[string]float64) *Rates {
	r.defaults = map[string]float64{}
	for cur, v := range kesPer {
		cur = strings.ToUpper(strings.TrimSpace(cur))
		if isCode(cur) && plausible(cur, v*probeAmount, v) == nil {
			r.defaults[cur] = v
		}
	}
	return r
}

// ParseDefaults reads "USD=129,EUR=140" into a map. Bad pairs are skipped.
func ParseDefaults(s string) map[string]float64 {
	out := map[string]float64{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || f <= 0 {
			continue
		}
		out[strings.ToUpper(strings.TrimSpace(k))] = f
	}
	return out
}

// KESPer is how many KES one unit of currency is worth: 1 for KES, else treasury's rate (cached,
// or the last one treasury gave while it cannot answer). It never uses the configured default,
// so money that is charged or launched always rests on a real treasury rate. Any failure returns
// an error wrapping ErrNoRate; a failure is never cached.
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
	return sharedcache.GetOrSetStale(ctx, r.cache, rateKey(cur), r.ttl, r.staleFor,
		func(ctx context.Context) (float64, error) { return r.read(ctx, cur) })
}

func rateKey(cur string) string   { return sharedcache.Key("marketflow", "fx", cur, "KES") }
func readAtKey(cur string) string { return sharedcache.Key("marketflow", "fxat", cur, "KES") }

// read fetches the rate and notes when, for ReadAt.
func (r *Rates) read(ctx context.Context, cur string) (float64, error) {
	v, err := r.fetch(ctx, cur)
	if err == nil && r.cache != nil && r.cache.Client() != nil {
		_ = r.cache.Client().Set(ctx, readAtKey(cur), time.Now().UTC().Format(time.RFC3339), r.ttl+r.staleFor).Err()
	}
	return v, err
}

// ReadAt is when the rate in use for currency was last read from treasury; false when unknown.
func (r *Rates) ReadAt(ctx context.Context, currency string) (time.Time, bool) {
	if r == nil || r.cache == nil || r.cache.Client() == nil {
		return time.Time{}, false
	}
	v, err := r.cache.Client().Get(ctx, readAtKey(strings.ToUpper(strings.TrimSpace(currency)))).Result()
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	return t, err == nil
}

// Refresh reads the rate from treasury now and replaces the shared cached one. The last rate stays
// in use when treasury cannot answer.
func (r *Rates) Refresh(ctx context.Context, currency string) error {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if cur == "KES" {
		return nil
	}
	if !isCode(cur) || r == nil || r.base == "" || r.key == "" || r.tenant == "" {
		return fmt.Errorf("%w: %s", ErrNoRate, cur)
	}
	v, err := r.read(ctx, cur)
	if err != nil {
		return err
	}
	if r.cache == nil {
		return nil
	}
	r.cache.Invalidate(ctx, rateKey(cur))
	_, err = sharedcache.GetOrSetStale(ctx, r.cache, rateKey(cur), r.ttl, r.staleFor,
		func(context.Context) (float64, error) { return v, nil })
	return err
}

// KESPerOrDefault is KESPer, then the configured default for reports and estimates. source is
// SourceTreasury or SourceDefault.
func (r *Rates) KESPerOrDefault(ctx context.Context, currency string) (rate float64, source string, err error) {
	rate, err = r.KESPer(ctx, currency)
	if err == nil {
		return rate, SourceTreasury, nil
	}
	if r != nil {
		if d, ok := r.defaults[strings.ToUpper(strings.TrimSpace(currency))]; ok {
			return d, SourceDefault, nil
		}
	}
	return 0, "", err
}

// Convert converts amount from one currency to another through KES (treasury's pivot), rounded
// at the target currency's places. Same currency returns the amount rounded. Strict like KESPer.
func (r *Rates) Convert(ctx context.Context, amount float64, from, to string) (float64, error) {
	return r.convert(ctx, amount, from, to, false)
}

// ConvertForDisplay is Convert that may use the configured default (reports and estimates).
func (r *Rates) ConvertForDisplay(ctx context.Context, amount float64, from, to string) (float64, error) {
	return r.convert(ctx, amount, from, to, true)
}

func (r *Rates) convert(ctx context.Context, amount float64, from, to string, allowDefault bool) (float64, error) {
	from, to = strings.ToUpper(strings.TrimSpace(from)), strings.ToUpper(strings.TrimSpace(to))
	if from == to {
		return money.RoundFor(to, amount), nil
	}
	read := func(cur string) (float64, error) {
		if allowDefault {
			v, _, err := r.KESPerOrDefault(ctx, cur)
			return v, err
		}
		return r.KESPer(ctx, cur)
	}
	fromKES, err := read(from)
	if err != nil {
		return 0, err
	}
	toKES, err := read(to)
	if err != nil {
		return 0, err
	}
	return money.RoundFor(to, amount*fromKES/toKES), nil
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
