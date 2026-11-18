package admin

import (
	"testing"
	"time"
)

func TestMonthRange(t *testing.T) {
	now := time.Date(2026, 11, 18, 21, 30, 0, 0, time.FixedZone("EAT", 3*3600))
	label, start, end, err := monthRange("", now)
	if err != nil || label != "2026-11" {
		t.Fatalf("default month = %q (%v), want 2026-11", label, err)
	}
	if !start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("default range = %v to %v", start, end)
	}
	// December rolls into the next year.
	label, start, end, err = monthRange("2026-12", now)
	if err != nil || label != "2026-12" || !start.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) ||
		!end.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("december = %q %v to %v (%v)", label, start, end, err)
	}
	// The first of the month in Nairobi is still the previous month in UTC.
	label, _, _, _ = monthRange("", time.Date(2026, 12, 1, 1, 0, 0, 0, time.FixedZone("EAT", 3*3600)))
	if label != "2026-11" {
		t.Fatalf("month is taken in UTC, got %q", label)
	}
	for _, bad := range []string{"2026-13", "2026-1", "11-2026", "2026-11-01", "abc", "1999-12"} {
		if _, _, _, err := monthRange(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParsePaging(t *testing.T) {
	if p, s, err := parsePaging("", ""); err != nil || p != 1 || s != 50 {
		t.Fatalf("defaults = %d, %d (%v)", p, s, err)
	}
	if p, s, err := parsePaging("3", "200"); err != nil || p != 3 || s != 200 {
		t.Fatalf("3, 200 = %d, %d (%v)", p, s, err)
	}
	for _, c := range [][2]string{{"0", ""}, {"-1", ""}, {"x", ""}, {"", "0"}, {"", "201"}, {"", "y"}} {
		if _, _, err := parsePaging(c[0], c[1]); err == nil {
			t.Errorf("page %q, page_size %q accepted", c[0], c[1])
		}
	}
}

func TestPriceMarginAndFloor(t *testing.T) {
	const fx, credit, floor = 130.0, 10.0, 0.30

	// 10 credits = KES 100 against USD 0.5 = KES 65: margin 0.35, above the floor.
	l := price(3, 10, 0.5, fx, credit, floor)
	if l.CreditsKES != 100 || l.MediaCostKES != 65 || l.MediaCostUSD != 0.5 || l.Versions != 3 || l.CreditsCharged != 10 {
		t.Fatalf("line = %+v", l)
	}
	if l.Margin == nil || *l.Margin != 0.35 || l.BelowFloor {
		t.Fatalf("margin = %v below %v, want 0.35 not below", l.Margin, l.BelowFloor)
	}

	// No credits charged: no margin, and any cost is flagged.
	l = price(1, 0, 0.2, fx, credit, floor)
	if l.Margin != nil || !l.BelowFloor {
		t.Fatalf("cost with no revenue: margin %v below %v", l.Margin, l.BelowFloor)
	}
	// Nothing charged and nothing spent is not flagged.
	l = price(0, 0, 0, fx, credit, floor)
	if l.Margin != nil || l.BelowFloor {
		t.Fatalf("empty: margin %v below %v", l.Margin, l.BelowFloor)
	}

	// Cost above revenue: a negative margin, flagged. KES 100 against USD 1 = KES 130.
	l = price(1, 10, 1, fx, credit, floor)
	if l.Margin == nil || *l.Margin != -0.3 || !l.BelowFloor {
		t.Fatalf("loss: margin %v below %v, want -0.3 flagged", l.Margin, l.BelowFloor)
	}

	// Exactly at the floor is not below it: KES 100 against KES 70 (USD 0.7 at 100).
	l = price(1, 10, 0.7, 100, credit, floor)
	if l.Margin == nil || *l.Margin != 0.3 || l.BelowFloor {
		t.Fatalf("at the floor: margin %v below %v, want 0.3 not flagged", l.Margin, l.BelowFloor)
	}
	// Just under the floor is flagged.
	l = price(1, 10, 0.71, 100, credit, floor)
	if l.Margin == nil || *l.Margin != 0.29 || !l.BelowFloor {
		t.Fatalf("under the floor: margin %v below %v, want 0.29 flagged", l.Margin, l.BelowFloor)
	}
}

func TestDecodeRegistryPricing(t *testing.T) {
	// Nothing stored: marketflow-ai's defaults.
	p, err := decodeRegistryPricing("")
	if err != nil || p.FXRate != 129 || p.MarginFloor != 0.30 || len(p.Bundles) != 4 {
		t.Fatalf("defaults = %+v (%v)", p, err)
	}
	if v := creditValueKES(p.Bundles); v != 10 {
		t.Fatalf("default credit value = %v, want 10 (the cheapest per credit)", v)
	}

	// A stored override: the fields it sets win, the rest keep the defaults, and fields the
	// report does not read are ignored.
	p, err = decodeRegistryPricing(`{"settings":{"fx_rate":140,"margin_floor":0.4,"credit_cost_basis":6},` +
		`"outputs":{"poster":{"cost_usd":0.05}},"bundles":[{"code":"b100","units":100,"price_kes":1200},{"code":"b500","units":500,"price_kes":5000}]}`)
	if err != nil || p.FXRate != 140 || p.MarginFloor != 0.4 || len(p.Bundles) != 2 {
		t.Fatalf("override = %+v (%v)", p, err)
	}
	if v := creditValueKES(p.Bundles); v != 10 {
		t.Fatalf("credit value = %v, want 10", v)
	}

	// Settings only: the bundles stay the defaults, an unset margin floor stays 0.30.
	p, err = decodeRegistryPricing(`{"settings":{"fx_rate":135}}`)
	if err != nil || p.FXRate != 135 || p.MarginFloor != 0.30 || len(p.Bundles) != 4 {
		t.Fatalf("settings only = %+v (%v)", p, err)
	}

	// Implausible values keep the defaults; so does an unreadable value (with an error).
	p, _ = decodeRegistryPricing(`{"settings":{"fx_rate":0,"margin_floor":1.5}}`)
	if p.FXRate != 129 || p.MarginFloor != 0.30 {
		t.Fatalf("implausible = %+v", p)
	}
	p, err = decodeRegistryPricing(`{not json`)
	if err == nil || p.FXRate != 129 || p.MarginFloor != 0.30 {
		t.Fatalf("unreadable = %+v (%v)", p, err)
	}

	// Bundles with no usable price are skipped; none at all is zero.
	if v := creditValueKES([]creditBundle{{Units: 0, PriceKES: 10}, {Units: 10, PriceKES: 0}}); v != 0 {
		t.Fatalf("no usable bundle = %v", v)
	}
}
