package contacts

import (
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/bengobox/marketflow-api/internal/platform/search"
)

// matchQuery is the one contact search rule for the list and the S2S search: every word of q must
// appear in the first name, last name, email or phone. "Test Customer" therefore finds the
// contact whose first name is Test and last name is Customer, which a whole-string match on each
// column never did. Returns nil when q has no words, meaning no filter.
func matchQuery(q string) predicate.Contact {
	terms := search.Terms(q)
	if len(terms) == 0 {
		return nil
	}
	each := make([]predicate.Contact, 0, len(terms))
	for _, t := range terms {
		each = append(each, contact.Or(
			contact.FirstNameContainsFold(t),
			contact.LastNameContainsFold(t),
			contact.EmailContainsFold(t),
			contact.PhoneContainsFold(t),
		))
	}
	return contact.And(each...)
}
