package testimonials

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

func TestBecamePublished(t *testing.T) {
	published := &ent.Testimonial{IsPublished: true}
	hidden := &ent.Testimonial{IsPublished: false}
	cases := []struct {
		name string
		was  bool
		t    *ent.Testimonial
		want bool
	}{
		{"created published", false, published, true},
		{"switched to published", false, published, true},
		{"edited while published", true, published, false},
		{"still hidden", false, hidden, false},
		{"unpublished", true, hidden, false},
	}
	for _, c := range cases {
		if got := becamePublished(c.was, c.t); got != c.want {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPublishedEventCarriesNoReviewText(t *testing.T) {
	tm := &ent.Testimonial{ID: uuid.New(), TenantID: uuid.New(), AuthorName: "Wanjiru", Body: "Great cakes", Rating: 5, IsPublished: true}
	ev := publishedEvent(tm)
	if ev == nil {
		t.Fatal("an event is built")
	}
	if TestimonialPublishedTopic != "marketflow.testimonial.published" {
		t.Fatalf("topic %q must stay inside the worker stream's marketflow.> binding", TestimonialPublishedTopic)
	}
	p := ev.Payload
	if p["testimonial_id"] != tm.ID.String() || p["rating"] != 5 {
		t.Fatalf("payload: %v", p)
	}
	for _, k := range []string{"body", "author_name"} {
		if _, ok := p[k]; ok {
			t.Fatalf("the event must not carry %s", k)
		}
	}
}
