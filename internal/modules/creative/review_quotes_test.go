package creative

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bengobox/marketflow-api/internal/ent"
)

func TestRankReviewsUnratedNeverOutranksRealFiveStars(t *testing.T) {
	site := &ent.Testimonial{AuthorName: "site", Rating: 5, DisplayOrder: 0, Metadata: map[string]any{"rating_unknown": true}}
	five := &ent.Testimonial{AuthorName: "five", Rating: 5, DisplayOrder: 9}
	four := &ent.Testimonial{AuthorName: "four", Rating: 4, DisplayOrder: 1}
	three := &ent.Testimonial{AuthorName: "three", Rating: 3, DisplayOrder: 0}
	rows := []*ent.Testimonial{three, site, four, five}
	rankReviews(rows)
	var got []string
	for _, r := range rows {
		got = append(got, r.AuthorName)
	}
	if strings.Join(got, ",") != "five,four,site,three" {
		t.Fatalf("order %v", got)
	}
}

func TestReviewLineKeepsWholeSentencesAndMarksRating(t *testing.T) {
	long := strings.Repeat("Great service from the whole team here. ", 12) // ~480 runes
	line := reviewLine(&ent.Testimonial{Body: long, AuthorName: "Akinyi", AuthorRole: "Parent", Rating: 5, Metadata: map[string]any{"rating_unknown": true}})
	words := strings.SplitN(line, " | ", 2)[0]
	if !strings.HasSuffix(words, "here.") || utf8.RuneCountInString(words) > maxQuoteRunes || strings.Contains(words, "…") {
		t.Fatalf("quote %q", words)
	}
	if !strings.HasSuffix(line, "| Akinyi, Parent | no star rating given") {
		t.Fatalf("line %q", line)
	}
	if len(line) > 600 {
		t.Fatalf("line is %d bytes, over marketflow-ai's 600", len(line))
	}
	if got := reviewLine(&ent.Testimonial{Body: "Loved it!", AuthorName: "Otieno", Rating: 4}); got != "Loved it! | Otieno | rated 4/5" {
		t.Fatalf("short %q", got)
	}
	// One run-on sentence longer than the limit cannot be quoted whole, so it is not sent.
	if got := reviewLine(&ent.Testimonial{Body: strings.Repeat("word ", 80), AuthorName: "x", Rating: 5}); got != "" {
		t.Fatalf("run-on kept as %q", got)
	}
}
