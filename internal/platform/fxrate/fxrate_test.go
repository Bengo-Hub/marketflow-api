package fxrate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// treasury answers convert from the rates map (KES per unit); a missing pair is a 500, as
// treasury answers "no exchange rate found".
func treasury(t *testing.T, rates map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v1/s2s/codevertex/exchange-rates/convert" || r.Header.Get("X-API-Key") != "k" ||
			q.Get("to") != "KES" || q.Get("amount") != "1000000" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		converted, ok := rates[q.Get("from")]
		if !ok {
			http.Error(w, `{"error":"failed to convert amount"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"from":"` + q.Get("from") + `","to":"KES","amount":"1000000","converted":"` + converted + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestKESPer(t *testing.T) {
	srv := treasury(t, map[string]string{"USD": "129500000", "UGX": "34700", "EUR": "1000000"})
	r := New(srv.URL, "k", "codevertex", nil)
	ctx := context.Background()

	if v, err := r.KESPer(ctx, "kes"); err != nil || v != 1 {
		t.Fatalf("KES is 1 without a call: %v %v", v, err)
	}
	if v, err := r.KESPer(ctx, " usd "); err != nil || v != 129.5 {
		t.Fatalf("USD from treasury: %v %v", v, err)
	}
	if v, err := r.KESPer(ctx, "UGX"); err != nil || v != 0.0347 {
		t.Fatalf("a small currency keeps its precision: %v %v", v, err)
	}
	for _, cur := range []string{"XYZ", "EUR", "", "US", "U$D"} {
		if _, err := r.KESPer(ctx, cur); !errors.Is(err, ErrNoRate) {
			t.Errorf("%q: want ErrNoRate, got %v", cur, err)
		}
	}
}

func TestKESPerFailsSafe(t *testing.T) {
	ctx := context.Background()
	srv := treasury(t, map[string]string{"USD": "1000"}) // 0.001 KES per USD: nonsense
	if _, err := New(srv.URL, "k", "codevertex", nil).KESPer(ctx, "USD"); !errors.Is(err, ErrNoRate) {
		t.Fatalf("an implausible USD rate is not used: %v", err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	down.Close()
	if _, err := New(down.URL, "k", "codevertex", nil).KESPer(ctx, "USD"); !errors.Is(err, ErrNoRate) {
		t.Fatalf("treasury down: %v", err)
	}
	if _, err := New("", "", "codevertex", nil).KESPer(ctx, "USD"); !errors.Is(err, ErrNoRate) {
		t.Fatalf("no treasury configured: %v", err)
	}
	var nilRates *Rates
	if _, err := nilRates.KESPer(ctx, "USD"); !errors.Is(err, ErrNoRate) {
		t.Fatalf("nil source: %v", err)
	}
}

func TestConvertPivotsThroughKES(t *testing.T) {
	srv := treasury(t, map[string]string{"USD": "129000000", "UGX": "34700"})
	r := New(srv.URL, "k", "codevertex", nil)
	ctx := context.Background()
	if v, err := r.Convert(ctx, 10, "USD", "KES"); err != nil || v != 1290 {
		t.Fatalf("USD to KES: %v %v", v, err)
	}
	if v, err := r.Convert(ctx, 1290, "kes", "usd"); err != nil || v != 10 {
		t.Fatalf("KES to USD: %v %v", v, err)
	}
	// 1 USD = 129 KES = 3717.6 UGX, rounded at UGX's 0 places.
	if v, err := r.Convert(ctx, 1, "USD", "UGX"); err != nil || v != 3718 {
		t.Fatalf("USD to UGX through KES: %v %v", v, err)
	}
	if v, err := r.Convert(ctx, 10.555, "KES", "KES"); err != nil || v != 10.56 {
		t.Fatalf("same currency is only rounded: %v %v", v, err)
	}
	if _, err := r.Convert(ctx, 1, "EUR", "KES"); !errors.Is(err, ErrNoRate) {
		t.Fatalf("no EUR rate: %v", err)
	}
}

func TestDefaultsOnlyForDisplay(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	down.Close()
	r := New(down.URL, "k", "codevertex", nil).WithDefaults(ParseDefaults("USD=129, eur=140,bad,XYZ=0,GBP=x"))
	ctx := context.Background()
	if _, err := r.KESPer(ctx, "USD"); !errors.Is(err, ErrNoRate) {
		t.Fatalf("a charge never uses the default: %v", err)
	}
	if _, err := r.Convert(ctx, 1, "USD", "KES"); !errors.Is(err, ErrNoRate) {
		t.Fatalf("strict convert never uses the default: %v", err)
	}
	if v, src, err := r.KESPerOrDefault(ctx, "usd"); err != nil || v != 129 || src != SourceDefault {
		t.Fatalf("report default: %v %q %v", v, src, err)
	}
	if v, err := r.ConvertForDisplay(ctx, 2, "EUR", "KES"); err != nil || v != 280 {
		t.Fatalf("display convert with default: %v %v", v, err)
	}
	if _, _, err := r.KESPerOrDefault(ctx, "GBP"); !errors.Is(err, ErrNoRate) {
		t.Fatalf("no default for GBP: %v", err)
	}
	if got := ParseDefaults("USD=129,bad,XYZ=0"); len(got) != 1 || got["USD"] != 129 {
		t.Fatalf("ParseDefaults: %v", got)
	}
}

func TestLastRateWhileTreasuryIsDown(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cache := sharedcache.New(rdb, zap.NewNop())
	calls, up := 0, true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"converted":"129000000"}`))
	}))
	t.Cleanup(srv.Close)
	r := New(srv.URL, "k", "codevertex", cache).WithCachePolicy(time.Minute, time.Hour)
	ctx := context.Background()
	if v, err := r.KESPer(ctx, "USD"); err != nil || v != 129 {
		t.Fatalf("first read: %v %v", v, err)
	}
	if v, err := r.KESPer(ctx, "USD"); err != nil || v != 129 || calls != 1 {
		t.Fatalf("cached read: %v %v calls=%d", v, err, calls)
	}
	up = false
	mr.FastForward(2 * time.Minute) // past the ttl, inside the stale window
	if v, err := r.KESPer(ctx, "USD"); err != nil || v != 129 {
		t.Fatalf("treasury down: the last rate is used: %v %v", v, err)
	}
	mr.FastForward(2 * time.Hour) // past ttl+staleFor: nothing left
	if _, err := r.KESPer(ctx, "USD"); !errors.Is(err, ErrNoRate) {
		t.Fatalf("no rate once the stale window is over: %v", err)
	}
}

// Sync now (plan item 75): a manual refresh replaces the fresh cached rate and its read time, and
// a treasury that cannot answer leaves the last rate in use.
func TestRefresh(t *testing.T) {
	mr := miniredis.RunT(t)
	cache := sharedcache.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}), zap.NewNop())
	converted, up := "129000000", true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"converted":"` + converted + `"}`))
	}))
	t.Cleanup(srv.Close)
	r := New(srv.URL, "k", "codevertex", cache)
	ctx := context.Background()
	if _, ok := r.ReadAt(ctx, "USD"); ok {
		t.Fatal("a rate nobody read has no read time")
	}
	if v, err := r.KESPer(ctx, "USD"); err != nil || v != 129 {
		t.Fatalf("first read: %v %v", v, err)
	}
	if at, ok := r.ReadAt(ctx, "usd"); !ok || time.Since(at) > time.Minute {
		t.Fatalf("read time: %v %v", at, ok)
	}
	converted = "131000000"
	if v, _ := r.KESPer(ctx, "USD"); v != 129 {
		t.Fatalf("still the cached rate: %v", v)
	}
	if err := r.Refresh(ctx, "USD"); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.KESPer(ctx, "USD"); v != 131 {
		t.Fatalf("after Sync now: %v", v)
	}
	up = false
	if err := r.Refresh(ctx, "USD"); err == nil {
		t.Fatal("treasury is down")
	}
	if v, _ := r.KESPer(ctx, "USD"); v != 131 {
		t.Fatalf("the last rate stays: %v", v)
	}
}
