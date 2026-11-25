package shareddefaults

import "encoding/json"

// Forever Business Owner (FBO) profile defaults, inherited by every Forever tenant (Meta). The
// business can set its own in Settings, Profile. The owner can also change a default for everyone
// in the platform service config (KeyForever).
const (
	DefaultFBOTagline      = "Forever Business Owner."
	DefaultFBOBio          = "Forever Living Products is the largest grower, manufacturer and distributor of Aloe Vera products in the world. For over 46 years, we have been dedicated to seeking out the best sources from nature for health and vitality, and sharing them with the world."
	DefaultFBOStory        = "Forever Living Products was founded in 1978 by Rex Maughan in Scottsdale, Arizona. We are the world's largest grower, manufacturer and distributor of aloe vera and bee products. Millions of people in over 160 countries have discovered the benefits of Forever’s aloe vera products to help them and their families look better and feel better. We own our own aloe fields, manufacturing and distribution facilities, and we utilize stabilized aloe vera gel because we control every step from plant to product to you."
	DefaultFBOVideoURL     = "https://www.youtube.com/embed/YRvN_JkHSzQ"
	DefaultFBOPrimaryColor = "#f59e0b"
	// Every Forever tenant uses the Forever Living logo, never Codevertex's (owner, 2026-11-19).
	// marketflow-ui serves it, like the featured product pictures.
	DefaultFBOLogo = "https://marketflow.codevertexafrica.com/images/public-profile/forever-logo.jpg"
	// LegacyFBOLogo is the Codevertex logo the defaults used before; a tenant still holding it
	// got it from the old default, not by choice.
	LegacyFBOLogo         = "https://accounts.codevertexafrica.com/images/logo/codevertex.png"
	DefaultFBOOpportunity = "Forever Business Owners are everyday people who choose to share products they enjoy. Some may earn supplemental income by building a customer base or team, but participation and effort vary from person to person. Our business model rewards individuals for product sales and for supporting others who choose to do the same.\n\nForever’s reward structure, known as the Marketing Plan, outlines the various roles, incentives, and bonuses available. It is a roadmap that explains how the business works and the requirements associated with each level. It offers tools and resources designed to support your activity, allowing you to work at your own pace."
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

// offeringsValue is DefaultFLPOfferings as the generic JSON shape stored in metadata ([]any of
// map[string]any), so it compares with an edited list.
func offeringsValue() []any {
	raw, _ := json.Marshal(DefaultFLPOfferings)
	var out []any
	_ = json.Unmarshal(raw, &out)
	return out
}

// codeDefaults is the shared set as shipped. A platform row (KeyForever) overrides single keys.
func codeDefaults() map[string]any {
	return map[string]any{
		"tagline":             DefaultFBOTagline,
		"bio":                 DefaultFBOBio,
		"story":               DefaultFBOStory,
		"video_url":           DefaultFBOVideoURL,
		"brand_primary_color": DefaultFBOPrimaryColor,
		"logo_url":            DefaultFBOLogo,
		"opportunity_text":    DefaultFBOOpportunity,
		"offerings":           offeringsValue(),
	}
}

// past are the values an earlier release shipped: a tenant still holding one got it as a copy of
// the default, so it follows the current one. Add the old value here whenever one above changes.
var past = map[string][]any{
	"logo_url": {LegacyFBOLogo},
}
