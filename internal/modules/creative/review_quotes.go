package creative

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// maxQuoteRunes is the longest review the script writer gets. A spoken quote longer than this
// does not fit a short ad, and marketflow-ai refuses lines over its own limit (600 bytes with the
// attribution), so a longer review is cut to its leading whole sentences, never mid-sentence:
// the writer is told to quote word for word, and a cut sentence would misquote the customer.
const maxQuoteRunes = 300

// reviewRating is a testimonial's real star rating, or 0 when nobody gave one (a quote found on
// the tenant's website is stored with the column default of 5 and metadata rating_unknown).
func reviewRating(t *ent.Testimonial) int {
	if unknown, _ := t.Metadata["rating_unknown"].(bool); unknown {
		return 0
	}
	return t.Rating
}

// rankReviews orders reviews for the script writer: real ratings of 4 or 5 first (best first),
// then unrated quotes, then low ratings; display order breaks ties. An unrated quote never
// outranks a customer who actually gave 5 stars.
func rankReviews(rows []*ent.Testimonial) {
	// 5 stars -> 0, 4 -> 1, unrated -> 2, 3 -> 3, 2 -> 4, 1 -> 5.
	tier := func(t *ent.Testimonial) int {
		switch r := reviewRating(t); {
		case r == 0:
			return 2
		case r >= 4:
			return 5 - r
		default:
			return 6 - r
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ti, tj := tier(rows[i]), tier(rows[j])
		if ti != tj {
			return ti < tj
		}
		return rows[i].DisplayOrder < rows[j].DisplayOrder
	})
}

// quotableExcerpt returns the review as the writer may quote it: whole when short enough, else
// its leading whole sentences up to maxQuoteRunes. "" when not even the first sentence fits.
func quotableExcerpt(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if utf8.RuneCountInString(body) <= maxQuoteRunes {
		return body
	}
	cut := ""
	for i, r := range body {
		if utf8.RuneCountInString(body[:i]) >= maxQuoteRunes {
			break
		}
		if r == '.' || r == '!' || r == '?' {
			end := i + utf8.RuneLen(r)
			if end == len(body) || body[end] == ' ' {
				cut = body[:end]
			}
		}
	}
	return strings.TrimSpace(cut)
}

// reviewLine is one review as the script writer reads it: the words, who said it, and the rating
// only when the customer gave one, so the writer never claims stars a quote does not have.
func reviewLine(t *ent.Testimonial) string {
	words := quotableExcerpt(t.Body)
	if words == "" {
		return ""
	}
	who := strings.TrimSpace(t.AuthorName)
	if role := strings.TrimSpace(t.AuthorRole); role != "" {
		who += ", " + role
	}
	line := words + " | " + excerpt(who, 120)
	if r := reviewRating(t); r > 0 {
		line += " | rated " + strconv.Itoa(r) + "/5"
	} else {
		line += " | no star rating given"
	}
	return line
}
