package fxrate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
