package tenant

import (
	"net/http"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

// BusinessTypeInfo is one business type an owner can pick (Sprint 7 #32).
type BusinessTypeInfo struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// AdminOnly types are set and removed by a platform admin only (FLP, owner 2026-11-14); the
	// owner's picker hides them.
	AdminOnly bool `json:"admin_only,omitempty"`
	// BannedClaims seed the brand context's banned claims in the onboarding draft: the type's
	// rules in the owner's words.
	BannedClaims []string `json:"-"`
}

// BusinessTypes mirrors the list in marketflow-ai (internal/compliance/data/business_types.yaml),
// which owns the rules: the compliance filter and the prompt guidance for every type live there,
// and an unknown type gets the generic rules. This copy only validates what the API stores and
// feeds the pickers and the onboarding draft. TestBusinessTypesMatchMarketflowAI compares the keys
// and labels with marketflow-ai's file when a checkout is at hand (MARKETFLOW_AI_DIR or a sibling
// marketflow-ai directory).
//
// A mirror rather than a call to marketflow-ai: validating a profile save must not depend on
// another service answering, the list changes only with a code change in both repos, and a key
// the AI does not know yet is still safe (it gets the generic rules).
var BusinessTypes = []BusinessTypeInfo{
	{Key: BusinessTypeGeneric, Label: "Other business",
		Description: "Any business not listed here. It follows the shared honesty rules every business gets."},
	{Key: BusinessTypeFLP, Label: "Forever Living (FBO)",
		Description: "Forever Business Owners selling Forever Living products. The strictest rules, with the health and income disclaimers Forever requires.",
		AdminOnly:   true,
		BannedClaims: []string{
			"Saying a product cures, treats, heals or prevents any disease",
			"Income promises: guaranteed income, get rich, passive income, financial freedom",
			"Income figures or screenshots of earnings",
			"Calling products FDA approved, a miracle or a wonder drug",
			"Using Forever Living logos without authorisation",
		}},
	{Key: "restaurant", Label: "Restaurant or cafe",
		Description: "Restaurants, cafes, bakeries, bars and food delivery. Honest menus, prices and allergens.",
		BannedClaims: []string{
			"Saying a dish or drink treats, prevents or heals a condition, burns fat or detoxes the body",
			"Calling food allergen free, nut free or gluten free without checking",
		}},
	{Key: "retail", Label: "Retail shop",
		Description: "Shops, supermarkets, boutiques and hardware stores selling in person. Real prices, real stock and real discounts.",
		BannedClaims: []string{
			"A \"was\" price the item never sold at",
			"Invented stock pressure such as \"only a few left\"",
			"\"Cheapest in Kenya\" or lowest price guarantees",
			"\"No refunds, no returns\" on faulty goods",
		}},
	{Key: "ecommerce", Label: "Online shop (e-commerce)",
		Description: "Businesses that sell online with delivery. Real prices and stock, clear delivery, payment and return terms.",
		BannedClaims: []string{
			"A \"was\" price the item never sold at",
			"Invented stock pressure, visitor counts or countdown timers",
			"\"Cheapest in Kenya\" or lowest price guarantees",
			"Hidden delivery costs or return terms",
		}},
	{Key: "services", Label: "Professional services",
		Description: "Consultants, agencies, trainers, contractors, cleaners and other service providers. No guaranteed outcomes, licences named truthfully.",
		BannedClaims: []string{
			"Guaranteed outcomes such as visa approval, a job, an exam pass or winning a case",
			"Licences, registrations or memberships the business does not hold",
		}},
	{Key: "saas", Label: "Software or app (SaaS)",
		Description: "Software, apps and online tools sold by subscription. No fake uptime or security claims, clear trial and renewal terms.",
		BannedClaims: []string{
			"100% uptime or \"never goes down\"",
			"\"Unhackable\", \"completely secure\" or certifications the business does not hold",
			"Hiding the renewal price or how to cancel a trial",
		}},
	{Key: "salon", Label: "Salon, barber or beauty",
		Description: "Hair salons, barbers, spas, nail and beauty studios. No guaranteed results, real before and after pictures only.",
		BannedClaims: []string{
			"Guaranteed, instant or permanent results",
			"Before and after pictures without the client's consent, or edited ones",
			"Skin lightening or bleaching products",
			"Hair regrowth or other medical claims",
		}},
	{Key: "pharmacy", Label: "Pharmacy or chemist",
		Description: "Pharmacies, chemists and health product shops. Pharmacy and Poisons Board advertising limits apply.",
		BannedClaims: []string{
			"Cure or guaranteed result claims, or \"no side effects\"",
			"Promoting prescription-only medicines to the public",
			"\"No prescription needed\"",
			"Treatment claims for serious diseases such as diabetes, cancer or HIV",
		}},
	{Key: "hospitality", Label: "Hotel, guest house or lodge",
		Description: "Hotels, guest houses, lodges, Airbnbs and camps. Prices say whether taxes are included, no fake availability.",
		BannedClaims: []string{
			"Invented availability such as \"only 2 rooms left\" or \"almost fully booked\"",
			"Prices that hide VAT or the catering levy",
			"A star rating the Tourism Regulatory Authority did not give",
		}},
}

// LookupBusinessType returns the type for a key, ignoring case and spaces.
func LookupBusinessType(key string) (BusinessTypeInfo, bool) {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, t := range BusinessTypes {
		if t.Key == k {
			return t, true
		}
	}
	return BusinessTypeInfo{}, false
}

// businessTypeChangeAllowed: Forever Living is admin-only (owner, 2026-11-14). Only a platform
// owner may switch a tenant to "flp" or away from it; saving the same type again, or moving
// between the other types, is open to the tenant's admins.
func businessTypeChangeAllowed(current, next string, platformOwner bool) bool {
	if platformOwner {
		return true
	}
	cur := strings.ToLower(strings.TrimSpace(current))
	if cur == next {
		return true
	}
	return cur != BusinessTypeFLP && next != BusinessTypeFLP
}

// callerIsPlatformOwner reports a signed-in MarketFlow platform owner (the same claim the /admin
// routes require).
func callerIsPlatformOwner(r *http.Request) bool {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	return ok && claims != nil && claims.IsPlatformOwner
}

// NormalizeBusinessType returns the stored form of a known key ("Pharmacy " -> "pharmacy").
func NormalizeBusinessType(key string) (string, bool) {
	t, ok := LookupBusinessType(key)
	return t.Key, ok
}
