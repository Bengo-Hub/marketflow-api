package flp

import (
	"fmt"
	"net/url"
	"strings"
)

// LinkBuilder constructs FLP-specific URLs for an FBO tenant.
// For FBO tenants, the tenant slug IS the distributor ID (e.g., "254200047984").
type LinkBuilder struct {
	DistribID   string // e.g., "254200047984" — also used as tenant slug for FBOs
	StoreCode   string // e.g., "KEN"
	Language    string // e.g., "en"
	TenantSlug  string // For FBOs: same as DistribID. For generic: business slug.
}

const (
	shopBaseURL    = "https://shop.foreverliving.com/retail/entry/Shop.do"
	recruitBaseURL = "https://join.foreverliving.com/marketing/joinnow/applicationForm.do"
	// MarketFlow hosts FBO profile pages — not redirecting to FLP's basic profile
	profileBaseURL = "https://marketflow.codevertexafrica.com/p/%s"
)

// ShopURL returns the FBO's personalized store landing page.
// Example: https://shop.foreverliving.com/retail/entry/Shop.do?store=KEN&language=en&distribID=254200047984
func (b *LinkBuilder) ShopURL() string {
	params := url.Values{}
	params.Set("store", b.StoreCode)
	params.Set("language", b.Language)
	params.Set("distribID", b.DistribID)
	return shopBaseURL + "?" + params.Encode()
}

// ProductURL returns a direct link to a specific product with the FBO's referral ID.
// Example: https://shop.foreverliving.com/retail/entry/Shop.do?store=KEN&language=en&itemCode=633&distribID=254200047984
func (b *LinkBuilder) ProductURL(itemCode string) string {
	params := url.Values{}
	params.Set("store", b.StoreCode)
	params.Set("language", b.Language)
	params.Set("itemCode", itemCode)
	params.Set("distribID", b.DistribID)
	return shopBaseURL + "?" + params.Encode()
}

// CategoryURL returns a link to a product category with the FBO's referral ID.
// Example: https://shop.foreverliving.com/retail/entry/Shop.do?store=KEN&language=en&categoryName=NutritionalSupplements&distribID=254200047984
func (b *LinkBuilder) CategoryURL(categoryName string) string {
	params := url.Values{}
	params.Set("store", b.StoreCode)
	params.Set("language", b.Language)
	params.Set("categoryName", categoryName)
	params.Set("distribID", b.DistribID)
	return shopBaseURL + "?" + params.Encode()
}

// RecruitURL returns the FBO's personalized join/recruit form link.
// Example: https://join.foreverliving.com/marketing/joinnow/applicationForm.do?action=display&store=KEN&distribID=254200047984&menuoff=n&langID=en
func (b *LinkBuilder) RecruitURL() string {
	params := url.Values{}
	params.Set("action", "display")
	params.Set("store", b.StoreCode)
	params.Set("distribID", b.DistribID)
	params.Set("menuoff", "n")
	params.Set("langID", b.Language)
	return recruitBaseURL + "?" + params.Encode()
}

// ProfileURL returns the FBO's MarketFlow-hosted profile page.
// For FBO tenants, slug = distribID, so profile URL uses the distribID directly.
// Example: https://marketflow.codevertexafrica.com/p/254200047984
func (b *LinkBuilder) ProfileURL() string {
	slug := b.TenantSlug
	if slug == "" {
		slug = b.DistribID // FBO fallback: use distribID as slug
	}
	return fmt.Sprintf(profileBaseURL, slug)
}

// WhatsAppURL returns a wa.me deep link for the FBO's WhatsApp number.
// Example: https://wa.me/254712345678?text=Hi%20I%27m%20interested%20in%20your%20products
func WhatsAppURL(phoneNumber string, prefilledMessage string) string {
	u := fmt.Sprintf("https://wa.me/%s", phoneNumber)
	if prefilledMessage != "" {
		u += "?text=" + url.QueryEscape(prefilledMessage)
	}
	return u
}

// ForTenant returns the link builder for an FLP tenant, the one place a Forever shop or join
// link is formed. The distributor ID is the stored one, else the slug when the slug is a
// distributor ID (FBO tenants are created under it). The store is the stored code, else the
// Forever store for the tenant's country (ISO 3166 alpha-2), else KEN. The language is the
// stored one, else en. ok is false when there is no distributor ID, so no link can be formed.
func ForTenant(slug, distribID, storeCode, language, country string) (b LinkBuilder, ok bool) {
	d := strings.TrimSpace(distribID)
	if d == "" && IsDistribID(slug) {
		d = slug
	}
	if d == "" {
		return LinkBuilder{}, false
	}
	return LinkBuilder{
		DistribID:  d,
		StoreCode:  StoreCode(storeCode, country),
		Language:   firstNonEmpty(strings.ToLower(strings.TrimSpace(language)), "en"),
		TenantSlug: slug,
	}, true
}

// StoreCode is the Forever store to use: the explicit code, else the country's store, else KEN.
func StoreCode(explicit, country string) string {
	if s := strings.ToUpper(strings.TrimSpace(explicit)); s != "" {
		return s
	}
	if s, ok := StoreCodesByCountry[strings.ToUpper(strings.TrimSpace(country))]; ok {
		return s
	}
	return "KEN"
}

// IsDistribID reports whether s looks like a Forever distributor ID: 8 or more digits.
func IsDistribID(s string) bool {
	if len(s) < 8 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// FLP product category names used in shop.foreverliving.com URLs.
var ProductCategories = map[string]string{
	"drinks":                 "AloeDrinks",
	"nutrition":              "NutritionalSupplements",
	"weight_management":      "WeightManagement",
	"bee_products":           "BeeProducts",
	"skin_care":              "SkinCare",
	"personal_care":          "PersonalCare",
	"essential_oils":         "EssentialOils",
	"combo_paks":             "ComboPaks",
}

// FLP store codes mapped from ISO country codes.
var StoreCodesByCountry = map[string]string{
	"KE": "KEN",
	"UG": "UGA",
	"TZ": "TZA",
	"NG": "NGA",
	"ZA": "ZAF",
	"GH": "GHA",
	"US": "USA",
	"GB": "GBR",
	"DE": "DEU",
	"FR": "FRA",
	"IN": "IND",
	"AE": "ARE",
}
