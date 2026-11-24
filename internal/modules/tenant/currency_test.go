package tenant

import "testing"

func TestResolveCurrency(t *testing.T) {
	ug, ke, zz := "UG", "ke", "ZZ"
	cases := []struct {
		meta     map[string]any
		country  *string
		platform string
		code     string
		source   string
	}{
		{map[string]any{"currency": "usd"}, &ke, "KES", "USD", CurrencyFromTenant},
		{map[string]any{"currency": "XYZ"}, &ug, "KES", "UGX", CurrencyFromCountry},
		{nil, &ke, "USD", "KES", CurrencyFromCountry},
		{nil, &zz, "USD", "USD", CurrencyFromPlatform},
		{nil, nil, "", "KES", CurrencyFromPlatform},
		{map[string]any{"currency": nil}, nil, "bad", "KES", CurrencyFromPlatform},
	}
	for i, c := range cases {
		code, src := ResolveCurrency(c.meta, c.country, c.platform)
		if code != c.code || src != c.source {
			t.Errorf("%d: got %s/%s, want %s/%s", i, code, src, c.code, c.source)
		}
	}
}

func TestValidateCurrencySetting(t *testing.T) {
	m := map[string]any{"currency": " ugx "}
	if err := ValidateCurrencySetting(m); err != nil || m["currency"] != "UGX" {
		t.Fatalf("normalised: %v %v", m, err)
	}
	m = map[string]any{"currency": ""}
	if err := ValidateCurrencySetting(m); err != nil || m["currency"] != nil {
		t.Fatalf("empty clears: %v %v", m, err)
	}
	for _, bad := range []any{"BTC", 5, "KESS"} {
		if err := ValidateCurrencySetting(map[string]any{"currency": bad}); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := ValidateCurrencySetting(map[string]any{"bio": "x"}); err != nil {
		t.Fatal(err)
	}
}
