package marketingos

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSiteReadsDescriptionAndSchema(t *testing.T) {
	page := `<html><head><title>Urban Loft Cafe, Busia</title>
<meta name="description" content="Specialty coffee and meals in Busia, Kenya, open every day from 7:30 AM to 10 PM.">
<script type="application/ld+json">{"@context":"https://schema.org","@type":"Restaurant","name":"The Urban Loft Cafe",
 "telephone":"+254116010638","address":{"@type":"PostalAddress","streetAddress":"Busia-Kisumu Road","addressLocality":"Busia","addressCountry":"KE"},
 "openingHoursSpecification":[{"@type":"OpeningHoursSpecification","dayOfWeek":["Monday","Sunday"],"opens":"07:30","closes":"22:00"}]}</script>
</head><body>menu</body></html>`
	got := parseSite(page)
	if !strings.Contains(got.Summary, "Busia, Kenya") || !strings.HasPrefix(got.Summary, "Urban Loft Cafe, Busia") {
		t.Fatalf("summary = %q", got.Summary)
	}
	if got.Address != "Busia-Kisumu Road, Busia, KE" || got.Phone != "+254116010638" {
		t.Fatalf("address %q phone %q", got.Address, got.Phone)
	}
	if len(got.Hours) != 1 || got.Hours[0] != "Monday, Sunday 07:30 to 22:00" {
		t.Fatalf("hours = %v", got.Hours)
	}
}

func TestParseSiteWithoutSchema(t *testing.T) {
	got := parseSite(`<title>Shop &amp; Co</title><meta property="og:description" content="Shoes in Kisumu">`)
	if got.Summary != "Shop & Co. Shoes in Kisumu" || got.Address != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestPublicOnlyRefusesInternalAddresses(t *testing.T) {
	for _, a := range []string{"127.0.0.1:443", "10.1.2.3:443", "192.168.0.5:443", "169.254.169.254:80", "[::1]:443", "0.0.0.0:443"} {
		if publicOnly("tcp", a, nil) == nil {
			t.Errorf("%s was allowed", a)
		}
	}
	if err := publicOnly("tcp", "104.21.10.92:443", nil); err != nil {
		t.Errorf("a public address was refused: %v", err)
	}
}

func TestReadSiteRefusesNonHTTPS(t *testing.T) {
	if _, err := readSite(t.Context(), "ftp://example.com"); err == nil {
		t.Fatal("ftp was read")
	}
	if _, err := readSite(t.Context(), "not a link"); err == nil {
		t.Fatal("a non-link was read")
	}
}

// The site's text carries offers, prices, services and policies, not only the address and hours,
// so it is read whenever the business has a website (owner, 2026-11-10).
func TestSiteTextReadWheneverThereIsAWebsite(t *testing.T) {
	calls := 0
	reader := func(_ context.Context, url string, max int) (string, error) {
		calls++
		if max != siteTextChars || url != "https://cafe.example" {
			t.Fatalf("reader got %q %d", url, max)
		}
		return "  Espresso KES 200. Busia, open every day 7:30 to 22:00.  ", nil
	}

	noSite := BusinessFacts{}
	addSiteText(context.Background(), &noSite, reader)
	if calls != 0 || noSite.SiteText != "" {
		t.Fatalf("read without a website: %d calls", calls)
	}

	withAddress := BusinessFacts{Website: "https://cafe.example", Address: "Busia", Sources: []string{"profile", "website"}}
	addSiteText(context.Background(), &withAddress, reader)
	if calls != 1 || withAddress.SiteText != "Espresso KES 200. Busia, open every day 7:30 to 22:00." || withAddress.Sources[len(withAddress.Sources)-1] != "crawler" {
		t.Fatalf("site text: calls %d facts %+v", calls, withAddress)
	}
}

func TestCrawlerFallbackFailureLeavesFactsAlone(t *testing.T) {
	failing := func(context.Context, string, int) (string, error) { return "", errors.New("down") }
	f := BusinessFacts{Website: "https://cafe.example", Sources: []string{"profile"}}
	addSiteText(context.Background(), &f, failing)
	addSiteText(context.Background(), &f, nil)
	if f.SiteText != "" || len(f.Sources) != 1 {
		t.Fatalf("a failed or missing reader changed the facts: %+v", f)
	}
}

func TestCrawlerReader(t *testing.T) {
	var gotKey, gotURL string
	var gotMax float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/fetch" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		gotKey = r.Header.Get("X-API-Key")
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotURL, _ = in["url"].(string)
		gotMax, _ = in["max_chars"].(float64)
		if gotURL == "https://blocked.example" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		_, _ = w.Write([]byte(`{"content":"Our menu","status":200}`))
	}))
	defer srv.Close()

	read := NewCrawlerReader(srv.URL, "k")
	text, err := read(context.Background(), "https://cafe.example", 1500)
	if err != nil || text != "Our menu" || gotKey != "k" || gotURL != "https://cafe.example" || gotMax != 1500 {
		t.Fatalf("read %q %v key %q url %q max %v", text, err, gotKey, gotURL, gotMax)
	}
	if _, err := read(context.Background(), "https://blocked.example", 1500); err == nil {
		t.Fatal("a refused page must be an error")
	}
	if NewCrawlerReader("", "k") != nil || NewCrawlerReader(srv.URL, "") != nil {
		t.Fatal("an unconfigured crawler must turn the fallback off")
	}
}
