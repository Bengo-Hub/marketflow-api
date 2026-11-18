package meetings

import (
	"context"
	"errors"
	"strings"
	"testing"

	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
	"github.com/google/uuid"
)

func TestNormalizeAttendeeEmail(t *testing.T) {
	ok := map[string]string{
		"jane@example.com":          "jane@example.com",
		"  Jane.Doe@Example.CO.KE ": "jane.doe@example.co.ke",
		"a+tag@mail.example.org":    "a+tag@mail.example.org",
	}
	for in, want := range ok {
		got, valid := NormalizeAttendeeEmail(in)
		if !valid || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, valid, want)
		}
	}
	bad := []string{
		"",
		"jane",
		"jane@localhost",
		"jane@.example.com",
		"jane@example..com",
		"jane@example.com.",
		"Jane <jane@example.com>",
		"jane@example.com, boss@example.com",
		"jane@example.com\r\nBcc: x@y.com",
		"a b@example.com",
		"@example.com",
		strings.Repeat("a", 65) + "@example.com",
		strings.Repeat("a", 60) + "@" + strings.Repeat("b", 200) + ".com",
	}
	for _, in := range bad {
		if _, valid := NormalizeAttendeeEmail(in); valid {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestBookableMeetingType(t *testing.T) {
	for _, k := range []string{"product_demo", "business_opportunity", "follow_up", "onboarding"} {
		if BookableMeetingType(k) != k {
			t.Errorf("%s changed", k)
		}
	}
	for _, k := range []string{"", "Free iPhone! click http://x", "PRODUCT_DEMO"} {
		if BookableMeetingType(k) != "product_demo" {
			t.Errorf("%q kept", k)
		}
	}
}

// fakeQuota counts uses per scope and feature, as ratelimit.Quota does for one day.
type fakeQuota struct {
	used   map[string]int
	scopes []string
	fail   bool
}

func (q *fakeQuota) Check(_ context.Context, scope, feature string, limit int) (*ratelimit.QuotaResult, error) {
	if q.fail {
		return nil, errors.New("redis down")
	}
	if q.used == nil {
		q.used = map[string]int{}
	}
	q.scopes = append(q.scopes, scope)
	k := scope + "|" + feature
	if q.used[k] >= limit {
		return &ratelimit.QuotaResult{Allowed: false, Feature: feature, Limit: limit, Used: q.used[k]}, nil
	}
	q.used[k]++
	return &ratelimit.QuotaResult{Allowed: true, Feature: feature, Limit: limit, Used: q.used[k]}, nil
}

func TestBookingGuardCaps(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	// Per invited address: the third invite to one address in a day is refused.
	q := &fakeQuota{}
	g := NewBookingGuard(q, BookingCaps{PerTenantDay: 100, PerEmailDay: 2, PerIPDay: 100}, nil)
	for i := 0; i < 2; i++ {
		if err := g.Allow(ctx, tenant, "jane@example.com", "203.0.113.7"); err != nil {
			t.Fatalf("invite %d: %v", i+1, err)
		}
	}
	if err := g.Allow(ctx, tenant, "jane@example.com", "203.0.113.8"); !errors.Is(err, ErrBookingLimit) {
		t.Fatalf("third invite to one address: %v", err)
	}
	// The address is counted by its hash; it never appears in a key.
	for _, s := range q.scopes {
		if strings.Contains(s, "jane") {
			t.Fatalf("raw address in a quota key: %s", s)
		}
	}

	// Per visitor address: one address booking many emails is stopped.
	q = &fakeQuota{}
	g = NewBookingGuard(q, BookingCaps{PerTenantDay: 100, PerEmailDay: 100, PerIPDay: 2}, nil)
	_ = g.Allow(ctx, tenant, "a@example.com", "203.0.113.7")
	_ = g.Allow(ctx, tenant, "b@example.com", "203.0.113.7")
	if err := g.Allow(ctx, tenant, "c@example.com", "203.0.113.7"); !errors.Is(err, ErrBookingLimit) {
		t.Fatalf("third booking from one address: %v", err)
	}
	// No visitor address (the AI tool through the API): the per-address cap does not apply.
	if err := g.Allow(ctx, tenant, "d@example.com", ""); err != nil {
		t.Fatalf("no visitor address: %v", err)
	}

	// Per tenant: many visitors and addresses still stop at the tenant's daily cap.
	q = &fakeQuota{}
	g = NewBookingGuard(q, BookingCaps{PerTenantDay: 3, PerEmailDay: 100, PerIPDay: 100}, nil)
	for i := 0; i < 3; i++ {
		if err := g.Allow(ctx, tenant, uuid.NewString()+"@example.com", uuid.NewString()); err != nil {
			t.Fatalf("booking %d: %v", i+1, err)
		}
	}
	if err := g.Allow(ctx, tenant, "z@example.com", "198.51.100.1"); !errors.Is(err, ErrBookingLimit) {
		t.Fatalf("fourth booking for the tenant: %v", err)
	}
	if err := g.Allow(ctx, uuid.New(), "z@example.com", "198.51.100.1"); err != nil {
		t.Fatalf("another tenant has its own cap: %v", err)
	}

	// 0 turns a cap off; a Redis error lets the booking through (the shared limiter's rule); a nil
	// guard or quota checks nothing.
	q = &fakeQuota{}
	g = NewBookingGuard(q, BookingCaps{}, nil)
	for i := 0; i < 5; i++ {
		if err := g.Allow(ctx, tenant, "jane@example.com", "203.0.113.7"); err != nil {
			t.Fatalf("caps off: %v", err)
		}
	}
	if len(q.scopes) != 0 {
		t.Fatalf("a cap set to 0 must not be counted: %v", q.scopes)
	}
	g = NewBookingGuard(&fakeQuota{fail: true}, BookingCaps{PerTenantDay: 1, PerEmailDay: 1, PerIPDay: 1}, nil)
	if err := g.Allow(ctx, tenant, "jane@example.com", "203.0.113.7"); err != nil {
		t.Fatalf("redis down: %v", err)
	}
	var nilGuard *BookingGuard
	if err := nilGuard.Allow(ctx, tenant, "jane@example.com", ""); err != nil {
		t.Fatalf("nil guard: %v", err)
	}
	if err := NewBookingGuard(nil, BookingCaps{PerTenantDay: 1}, nil).Allow(ctx, tenant, "x@example.com", ""); err != nil {
		t.Fatalf("nil quota: %v", err)
	}
}
