package tenant

import (
	"encoding/json"
	"strings"
)

// Forever Business Owner (FBO) profile defaults, applied when a tenant whose slug is a
// Forever distributor ID is first synced. The owner can change every one of them in
// Settings > Profile; a key that already exists is never replaced.
const (
	DefaultFBOTagline      = "Forever Business Owner."
	DefaultFBOBio          = "Forever Living Products is the largest grower, manufacturer and distributor of Aloe Vera products in the world. For over 46 years, we have been dedicated to seeking out the best sources from nature for health and vitality, and sharing them with the world."
	DefaultFBOStory        = "Forever Living Products was founded in 1978 by Rex Maughan in Scottsdale, Arizona. We are the world's largest grower, manufacturer and distributor of aloe vera and bee products. Millions of people in over 160 countries have discovered the benefits of Forever’s aloe vera products to help them and their families look better and feel better. We own our own aloe fields, manufacturing and distribution facilities, and we utilize stabilized aloe vera gel because we control every step from plant to product to you."
	DefaultFBOVideoURL     = "https://www.youtube.com/embed/YRvN_JkHSzQ"
	DefaultFBOPrimaryColor = "#f59e0b"
	DefaultFBOLogo         = "https://accounts.codevertexafrica.com/images/logo/codevertex.png"
	DefaultFBOOpportunity  = "Forever Business Owners are everyday people who choose to share products they enjoy. Some may earn supplemental income by building a customer base or team, but participation and effort vary from person to person. Our business model rewards individuals for product sales and for supporting others who choose to do the same.\n\nForever’s reward structure, known as the Marketing Plan, outlines the various roles, incentives, and bonuses available. It is a roadmap that explains how the business works and the requirements associated with each level. It offers tools and resources designed to support your activity, allowing you to work at your own pace."
)

// FLPOffering is one featured Forever product. The public page links it to the
// distributor's own Forever shop with the item code, built at render time from the
// tenant's distributor ID, store and language, so the link never goes stale.
type FLPOffering struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ImageURL    string `json:"image_url"`
	FLPItemCode string `json:"flp_item_code"`
	CC          string `json:"cc"`
}

// DefaultFLPOfferings are the featured products every FLP tenant starts with. Images
// are served by marketflow-ui, which renders /p/{slug}.
var DefaultFLPOfferings = []FLPOffering{
	{Name: "Aloe Vera Gel", FLPItemCode: "015", CC: "0.103", Description: "Pure stabilized inner leaf gel for digestive wellness and natural hydration.", ImageURL: "/images/public-profile/featured-products/aloe-vera-gel.jpg"},
	{Name: "Arctic Sea", FLPItemCode: "376", CC: "0.163", Description: "Omega-3 DHA & EPA for heart health, brain function, and joint support.", ImageURL: "/images/public-profile/featured-products/artic-sea.jpg"},
	{Name: "Forever Kids", FLPItemCode: "354", CC: "0.278", Description: "Chewable phytonutrient-rich multivitamins designed for growing children.", ImageURL: "/images/public-profile/featured-products/forever-kids.jpg"},
	{Name: "Avocado Soap", FLPItemCode: "284", CC: "0.113", Description: "Moisturizing soap with natural avocado oil and aloe vera for soft, nourished skin.", ImageURL: "/images/public-profile/featured-products/avocado-soap.jpg"},
	{Name: "Bright Toothgel", FLPItemCode: "028", CC: "0.080", Description: "Fluoride-free aloe vera toothgel for gentle whitening and lasting freshness.", ImageURL: "/images/public-profile/featured-products/tooth-gel.jpg"},
	{Name: "Forever Deodorant", FLPItemCode: "067", CC: "0.088", Description: "Aluminum-free aloe vera deodorant for all-day freshness without harsh chemicals.", ImageURL: "/images/public-profile/featured-products/deordorant.jpg"},
	{Name: "Aloe Lips", FLPItemCode: "022", CC: "0.046", Description: "Soothing aloe vera lip balm that moisturizes and protects lips all day long.", ImageURL: "/images/public-profile/featured-products/aloe-lips.jpg"},
}

// defaultOfferingsValue is DefaultFLPOfferings as the generic JSON shape stored in
// metadata ([]any of map[string]any), so it reads back the same as an edited list.
func defaultOfferingsValue() []any {
	raw, _ := json.Marshal(DefaultFLPOfferings)
	var out []any
	_ = json.Unmarshal(raw, &out)
	return out
}

// isFLPTenant decides the business type for a synced tenant: a Forever distributor ID
// as the slug, or auth metadata that says so. Everyone else is generic.
func isFLPTenant(slug string, remoteMeta map[string]any) bool {
	if looksLikeDistribID(slug) {
		return true
	}
	bt, _ := remoteMeta["business_type"].(string)
	return strings.EqualFold(strings.TrimSpace(bt), BusinessTypeFLP)
}

// applyFBODefaults fills the FBO profile defaults and the featured products into meta
// for keys that are not set yet. It never replaces an existing value.
func applyFBODefaults(meta map[string]any) map[string]any {
	if meta == nil {
		meta = make(map[string]any)
	}
	defaults := map[string]any{
		"tagline":             DefaultFBOTagline,
		"bio":                 DefaultFBOBio,
		"story":               DefaultFBOStory,
		"video_url":           DefaultFBOVideoURL,
		"brand_primary_color": DefaultFBOPrimaryColor,
		"logo_url":            DefaultFBOLogo,
		"opportunity_text":    DefaultFBOOpportunity,
	}
	for k, v := range defaults {
		if _, ok := meta[k]; !ok {
			meta[k] = v
		}
	}
	if _, ok := meta["offerings"]; !ok {
		meta["offerings"] = defaultOfferingsValue()
	}
	return meta
}

// mergeMissing copies keys from src that dst does not have. Local values always win:
// auth's copy of metadata must never overwrite profile edits or the brand context.
func mergeMissing(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}
