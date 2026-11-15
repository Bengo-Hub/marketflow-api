package enrichment

import (
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
)

func TestClassifyEmail(t *testing.T) {
	cases := map[string]*Email{
		"jane@gmail.com":      {Kind: "free"},
		"Jane@Yahoo.com ":     {Kind: "free"},
		"jane@acme.co.ke":     {Kind: "organisation", Domain: "acme.co.ke"},
		"ops@Sub.Example.ORG": {Kind: "organisation", Domain: "sub.example.org"},
		"no-at-sign":          nil,
		"jane@localhost":      nil,
		"jane@":               nil,
	}
	for in, want := range cases {
		got := classifyEmail(in)
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("%q: got %+v, want %+v", in, got, want)
		}
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://www.Acme.co.ke/about": "acme.co.ke",
		"acme.co.ke":                   "acme.co.ke",
		"http://shop.acme.com":         "shop.acme.com",
		"":                             "",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestSegmentFor(t *testing.T) {
	now := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)
	recent, old := stamp(now.AddDate(0, 0, -10)), stamp(now.AddDate(0, 0, -200))
	cases := []struct {
		name string
		p    Profile
		want string
	}{
		{"recent buyer", Profile{Purchases: Purchases{Orders: 2, LastPurchaseAt: recent}}, SegmentActiveCustomer},
		{"payment only", Profile{Purchases: Purchases{Payments: 1, LastPurchaseAt: recent}}, SegmentActiveCustomer},
		{"old buyer with open deal", Profile{Purchases: Purchases{Orders: 1, LastPurchaseAt: old}, Deals: Deals{Open: 1}}, SegmentLapsedCustomer},
		{"buyer without a date", Profile{Purchases: Purchases{Orders: 1}}, SegmentLapsedCustomer},
		{"open deal", Profile{Deals: Deals{Open: 1}, Engagement: Engaged{Activities90d: 3}}, SegmentInPipeline},
		{"talking", Profile{Engagement: Engaged{Activities90d: 1}}, SegmentEngaged},
		{"nothing", Profile{}, SegmentQuiet},
	}
	for _, c := range cases {
		if got := segmentFor(&c.p, now); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestKeepSummaryOnlyForTheSameSite(t *testing.T) {
	prev := &Profile{Company: &Company{Website: "https://www.acme.co.ke", Summary: "Acme. Hardware in Nairobi", SummaryReadAt: "2026-11-01T02:00:00Z"}}
	same := &Company{Website: "acme.co.ke"}
	keepSummary(same, prev)
	if same.Summary == "" || same.SummaryReadAt != "2026-11-01T02:00:00Z" {
		t.Fatalf("a failed read of the same site keeps the earlier summary: %+v", same)
	}
	other := &Company{Website: "other.co.ke"}
	keepSummary(other, prev)
	if other.Summary != "" {
		t.Fatalf("another site never inherits a summary: %+v", other)
	}
}

func TestPreviousReadsStoredProfile(t *testing.T) {
	c := &ent.Contact{Metadata: map[string]any{MetadataKey: map[string]any{
		"version": float64(1), "segment": "quiet",
		"company": map[string]any{"website": "acme.co.ke", "summary": "Acme"},
	}}}
	p := previous(c)
	if p == nil || p.Segment != "quiet" || p.Company == nil || p.Company.Summary != "Acme" {
		t.Fatalf("got %+v", p)
	}
	if previous(&ent.Contact{}) != nil {
		t.Fatal("no stored profile gives nil")
	}
}

func TestSwitchDefaults(t *testing.T) {
	cases := []struct {
		meta    map[string]any
		on, web bool
	}{
		{map[string]any{}, true, false},
		{map[string]any{marketingos.ContactEnrichmentKey: false}, false, false},
		{map[string]any{marketingos.ContactEnrichmentWebKey: true}, true, true},
		{map[string]any{marketingos.ContactEnrichmentKey: false, marketingos.ContactEnrichmentWebKey: true}, false, false},
	}
	for _, c := range cases {
		on, web := Switches(c.meta)
		if on != c.on || web != c.web {
			t.Errorf("%v: got %v %v, want %v %v", c.meta, on, web, c.on, c.web)
		}
	}
}

func TestErased(t *testing.T) {
	if !Erased(&ent.Contact{Metadata: map[string]any{"erased_at": "2026-11-11T00:00:00Z"}}) {
		t.Fatal("an erased contact is never enriched")
	}
	if Erased(&ent.Contact{}) {
		t.Fatal("a normal contact is enriched")
	}
}
