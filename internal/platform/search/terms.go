// Package search holds the query rules shared by the list and search endpoints.
package search

import "strings"

const (
	// MaxTerms caps how many words of a query become match conditions, so a pasted paragraph
	// cannot turn into a very wide SQL predicate.
	MaxTerms = 5
	// MaxTermLen caps each word.
	MaxTermLen = 64
)

// Terms splits a free-text query into the words every result must match. A person types a full
// name ("Test Customer") while the name sits in two columns, so each word is matched on its own
// and all of them must match somewhere. Empty and repeated words are dropped (case-insensitive).
func Terms(q string) []string {
	fields := strings.Fields(q)
	out := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if len(f) > MaxTermLen {
			f = f[:MaxTermLen]
		}
		key := strings.ToLower(f)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
		if len(out) == MaxTerms {
			break
		}
	}
	return out
}
