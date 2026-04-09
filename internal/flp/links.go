package flp

import (
	"fmt"
	"net/url"
)

// LinkBuilder constructs FLP-specific URLs for an FBO tenant.
type LinkBuilder struct {
	DistribID   string // e.g., "254200047984"
	StoreCode   string // e.g., "KEN"
	Language    string // e.g., "en"
	ProfileUUID string // e.g., "2c64be1f-f2cc-47a8-b357-39e9780d02e1"
}

const (
	shopBaseURL    = "https://shop.foreverliving.com/retail/entry/Shop.do"
	recruitBaseURL = "https://join.foreverliving.com/marketing/joinnow/applicationForm.do"
	profileBaseURL = "https://%s.flp.com/opportunity/%s"
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

// ProfileURL returns the FBO's personalized opportunity/profile website.
// Example: https://254000328986.flp.com/opportunity/2c64be1f-f2cc-47a8-b357-39e9780d02e1
func (b *LinkBuilder) ProfileURL() string {
	if b.ProfileUUID == "" {
		return ""
	}
	return fmt.Sprintf(profileBaseURL, b.DistribID, b.ProfileUUID)
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
