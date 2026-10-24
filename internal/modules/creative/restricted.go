package creative

import (
	"regexp"
	"sort"
	"strings"
)

// Restricted categories (gate G1): ads the platforms review more strictly. A hit
// does not block the poster; it is recorded on the version
// (metadata.restricted_categories) so the reviewer sees it before approving
// and the ad publisher can apply the stricter targeting rules (age limits,
// special ad categories).
var restrictedTerms = map[string][]string{
	"health":    {"cure", "cures", "treat", "treats", "heal", "heals", "weight loss", "lose weight", "diabetes", "cancer", "hypertension", "detox", "miracle", "guaranteed results"},
	"finance":   {"loan", "loans", "credit", "invest", "investment", "returns", "forex", "crypto", "double your money", "guaranteed income", "earn per day"},
	"alcohol":   {"beer", "wine", "whisky", "whiskey", "vodka", "gin", "spirits", "liquor", "happy hour"},
	"betting":   {"bet", "betting", "jackpot", "odds", "casino", "sportpesa", "gamble"},
	"politics":  {"vote", "election", "campaign rally", "candidate", "party manifesto"},
	"education": {"guaranteed job", "certified in", "accredited", "100% pass"},
}

var restrictedRe = func() map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(restrictedTerms))
	for cat, terms := range restrictedTerms {
		quoted := make([]string, len(terms))
		for i, t := range terms {
			quoted[i] = regexp.QuoteMeta(t)
		}
		out[cat] = regexp.MustCompile(`(?i)\b(` + strings.Join(quoted, "|") + `)\b`)
	}
	return out
}()

// restrictedCategories lists the categories the texts touch, sorted.
func restrictedCategories(texts ...string) []string {
	joined := strings.Join(texts, "\n")
	var hits []string
	for cat, re := range restrictedRe {
		if re.MatchString(joined) {
			hits = append(hits, cat)
		}
	}
	sort.Strings(hits)
	return hits
}
