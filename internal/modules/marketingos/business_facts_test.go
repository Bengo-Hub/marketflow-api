package marketingos

import (
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
