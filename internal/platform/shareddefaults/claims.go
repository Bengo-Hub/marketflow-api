package shareddefaults

import "strings"

// genericClaims are claims every business must avoid (Consumer Protection Act 2012).
var genericClaims = []string{
	"False or misleading claims about price, quality or results",
}

// typeClaims are each business type's own rules (Sprint 7 #34; the Forever Living ones follow
// docs/compliance-rules.md). marketflow-ai owns the compliance filter; these reach it as the
// business's banned claims.
var typeClaims = map[string][]string{
	"flp": {
		"Saying a product cures, treats, heals or prevents any disease",
		"Income promises: guaranteed income, get rich, passive income, financial freedom",
		"Income figures or screenshots of earnings",
		"Calling products FDA approved, a miracle or a wonder drug",
		"Using Forever Living logos without authorisation",
	},
	"restaurant": {
		"Saying a dish or drink treats, prevents or heals a condition, burns fat or detoxes the body",
		"Calling food allergen free, nut free or gluten free without checking",
	},
	"retail": {
		"A \"was\" price the item never sold at",
		"Invented stock pressure such as \"only a few left\"",
		"\"Cheapest in Kenya\" or lowest price guarantees",
		"\"No refunds, no returns\" on faulty goods",
	},
	"ecommerce": {
		"A \"was\" price the item never sold at",
		"Invented stock pressure, visitor counts or countdown timers",
		"\"Cheapest in Kenya\" or lowest price guarantees",
		"Hidden delivery costs or return terms",
	},
	"services": {
		"Guaranteed outcomes such as visa approval, a job, an exam pass or winning a case",
		"Licences, registrations or memberships the business does not hold",
	},
	"saas": {
		"100% uptime or \"never goes down\"",
		"\"Unhackable\", \"completely secure\" or certifications the business does not hold",
		"Hiding the renewal price or how to cancel a trial",
	},
	"salon": {
		"Guaranteed, instant or permanent results",
		"Before and after pictures without the client's consent, or edited ones",
		"Skin lightening or bleaching products",
		"Hair regrowth or other medical claims",
	},
	"pharmacy": {
		"Cure or guaranteed result claims, or \"no side effects\"",
		"Promoting prescription-only medicines to the public",
		"Discounts, offers or giveaways on any medicine, general sale medicines included",
		"Medicine adverts without the PPB advert approval reference",
		"\"No prescription needed\"",
		"Treatment claims for serious diseases such as diabetes, cancer or HIV",
	},
	"hospitality": {
		"Invented availability such as \"only 2 rooms left\" or \"almost fully booked\"",
		"Prices that hide VAT or the catering levy",
		"A star rating the Tourism Regulatory Authority did not give",
	},
}

// TypeClaims are the shared banned claims of a business type alone.
func TypeClaims(businessType string) []string {
	return typeClaims[strings.ToLower(strings.TrimSpace(businessType))]
}

// SharedClaims is the shared banned claims list a business inherits: the generic ones, its type's,
// and any a platform row adds (KeyClaims). The row can only add, so it never weakens the rules.
func SharedClaims(businessType string) []string {
	extra := current().claims
	return Union(genericClaims, TypeClaims(businessType), extra["all"], extra[strings.ToLower(strings.TrimSpace(businessType))])
}

// Union joins claim lists, dropping blanks and repeats (case and spacing ignored).
func Union(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, c := range l {
			k := claimKey(c)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, strings.TrimSpace(c))
		}
	}
	return out
}

// OwnClaims is list without the claims the business inherits: what it added itself.
func OwnClaims(businessType string, list []string) []string {
	shared := map[string]bool{}
	for _, c := range SharedClaims(businessType) {
		shared[claimKey(c)] = true
	}
	out := []string{}
	for _, c := range list {
		if k := claimKey(c); k != "" && !shared[k] {
			out = append(out, c)
		}
	}
	return out
}

func claimKey(c string) string { return strings.Join(strings.Fields(strings.ToLower(c)), " ") }
