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
	},
	{Key: "restaurant", Label: "Restaurant or cafe",
		Description: "Restaurants, cafes, bakeries, bars and food delivery. Honest menus, prices and allergens.",
	},
	{Key: "retail", Label: "Retail shop",
		Description: "Shops, supermarkets, boutiques and hardware stores selling in person. Real prices, real stock and real discounts.",
	},
	{Key: "ecommerce", Label: "Online shop (e-commerce)",
		Description: "Businesses that sell online with delivery. Real prices and stock, clear delivery, payment and return terms.",
	},
	{Key: "services", Label: "Professional services",
		Description: "Consultants, agencies, trainers, contractors, cleaners and other service providers. No guaranteed outcomes, licences named truthfully.",
	},
	{Key: "saas", Label: "Software or app (SaaS)",
		Description: "Software, apps and online tools sold by subscription. No fake uptime or security claims, clear trial and renewal terms.",
	},
	{Key: "salon", Label: "Salon, barber or beauty",
		Description: "Hair salons, barbers, spas, nail and beauty studios. No guaranteed results, real before and after pictures only.",
	},
	{Key: "pharmacy", Label: "Pharmacy or chemist",
		Description: "Pharmacies, chemists and health product shops. Pharmacy and Poisons Board advertising limits apply.",
	},
	{Key: "hospitality", Label: "Hotel, guest house or lodge",
		Description: "Hotels, guest houses, lodges, Airbnbs and camps. Prices say whether taxes are included, no fake availability.",
	},
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
