package testimonials

import (
	"net/url"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
)

const sitePage = `<html><head>
<script type="application/ld+json">{"@context":"https://schema.org","@type":"LocalBusiness","name":"Mama's Kitchen",
 "review":[{"@type":"Review","author":{"@type":"Person","name":"Achieng O."},"reviewBody":"The best pilau in Kisumu, and they deliver on time every single week.",
 "reviewRating":{"@type":"Rating","ratingValue":"5"}},
 {"@type":"Review","author":"Brian K","reviewBody":"Friendly staff and fair prices for a whole family meal.","reviewRating":{"ratingValue":8,"bestRating":10}}]}</script>
</head><body>
<section class="home-testimonials">
  <blockquote>“Catering for our office party was spotless, everyone asked who cooked.”<footer>Jane Wambui, HR Manager at Acme</footer></blockquote>
  <figure><blockquote>Their chapati is soft every time I order it for my kids.</blockquote><figcaption>— Peter Otieno | Parent</figcaption></figure>
  <blockquote>Too short</blockquote><p>Someone</p>
  <blockquote>A quote that nobody signed at all but is long enough to count.</blockquote>
</section>
<section><blockquote>A quote outside any testimonial section, from a blog post we must not take.</blockquote><p>Writer</p></section>
<a href="/testimonials">More reviews</a><a href="https://other.example.com/reviews">Elsewhere</a><a href="/menu">Menu</a>
</body></html>`

func TestExtractTestimonialsVerbatim(t *testing.T) {
	got := ExtractTestimonials(sitePage, "https://mamas.example.com/")
	want := map[string]Found{
		"Achieng O.":   {Body: "The best pilau in Kisumu, and they deliver on time every single week.", Rating: 5},
		"Brian K":      {Body: "Friendly staff and fair prices for a whole family meal.", Rating: 0},
		"Jane Wambui":  {Body: "Catering for our office party was spotless, everyone asked who cooked.", Role: "HR Manager at Acme"},
		"Peter Otieno": {Body: "Their chapati is soft every time I order it for my kids.", Role: "Parent"},
	}
	if len(got) != len(want) {
		t.Fatalf("found %d: %+v", len(got), got)
	}
	for _, f := range got {
		w, ok := want[f.Author]
		if !ok {
			t.Fatalf("unexpected author %q", f.Author)
		}
		if f.Body != w.Body || f.Rating != w.Rating || f.Role != w.Role || f.SourceURL != "https://mamas.example.com/" {
			t.Fatalf("%s: got %+v want %+v", f.Author, f, w)
		}
	}
}

func TestTestimonialLinksStayOnSite(t *testing.T) {
	base, _ := url.Parse("https://mamas.example.com/")
	links := testimonialLinks(sitePage, base)
	if len(links) != 1 || links[0] != "https://mamas.example.com/testimonials" {
		t.Fatalf("links: %v", links)
	}
}

func TestFingerprintIgnoresCaseAndPunctuation(t *testing.T) {
	a := Fingerprint("Jane Wambui", "Great food!  Fast delivery.")
	b := Fingerprint("jane wambui", "great food fast delivery")
	if a != b || len(a) != 64 {
		t.Fatalf("fingerprints differ: %s %s", a, b)
	}
	if Fingerprint("Jane", "Great food") == Fingerprint("John", "Great food") {
		t.Fatal("the author is part of the fingerprint")
	}
	if BodyFingerprint("Great food!") != BodyFingerprint("great food") {
		t.Fatal("body fingerprint")
	}
}

func TestEditAllowedKeepsCustomerWords(t *testing.T) {
	form := &ent.Testimonial{Source: SourceForm, Status: StatusPending, AuthorName: "Jane", Body: "Great food", Rating: 4}
	if code, _ := editAllowed(form, upsertRequest{Body: ptr("Great food and service")}); code != "words_locked" {
		t.Fatalf("body edit on a form testimonial: %q", code)
	}
	if code, _ := editAllowed(form, upsertRequest{Rating: ptr(5)}); code != "words_locked" {
		t.Fatalf("rating edit: %q", code)
	}
	if code, _ := editAllowed(form, upsertRequest{IsPublished: ptr(true)}); code != "not_approved" {
		t.Fatalf("publishing pending: %q", code)
	}
	if code, _ := editAllowed(form, upsertRequest{DisplayOrder: ptr(2), AuthorRole: ptr("Parent")}); code != "" {
		t.Fatalf("order and role stay editable: %q", code)
	}
	manual := &ent.Testimonial{Source: SourceManual, Status: StatusApproved, Body: "x"}
	if code, _ := editAllowed(manual, upsertRequest{Body: ptr("y"), IsPublished: ptr(true)}); code != "" {
		t.Fatalf("the tenant's own testimonial is editable: %q", code)
	}
}

func TestRatingOfUnknown(t *testing.T) {
	if ratingOf(&ent.Testimonial{Rating: 5, Metadata: map[string]any{"rating_unknown": true}}) != 0 {
		t.Fatal("a website quote without stars shows no rating")
	}
	if ratingOf(&ent.Testimonial{Rating: 4}) != 4 {
		t.Fatal("given rating")
	}
}

func TestSplitCredit(t *testing.T) {
	for in, want := range map[string][2]string{
		"— Jane Doe, Owner":   {"Jane Doe", "Owner"},
		"Jane Doe | Customer": {"Jane Doe", "Customer"},
		"Jane":                {"Jane", ""},
	} {
		a, r := splitCredit(in)
		if a != want[0] || r != want[1] {
			t.Fatalf("%q: %q %q", in, a, r)
		}
	}
	_ = strings.TrimSpace
}
