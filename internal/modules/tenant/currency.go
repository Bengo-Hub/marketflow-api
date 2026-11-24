package tenant

import (
	"context"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
)

// CurrencyMetadataKey is the tenant's own currency choice in tenant metadata (no schema field).
const CurrencyMetadataKey = "currency"

// Where a tenant's currency came from.
const (
	CurrencyFromTenant   = "tenant"   // the owner chose it in MarketFlow settings
	CurrencyFromCountry  = "country"  // the tenant's SSO country
	CurrencyFromPlatform = "platform" // MF_MONEY_DEFAULT_CURRENCY
)

// SupportedCurrencies mirrors shared-ui-lib's CURRENCY_META and treasury-api's supported list,
// so a tenant can only pick a currency every service formats and converts.
var SupportedCurrencies = []string{"KES", "USD", "EUR", "GBP", "UGX", "TZS", "ZAR", "NGN", "GHS", "RWF",
	"ETB", "EGP", "INR", "CNY", "JPY", "CAD", "AUD", "CHF", "AED", "SAR"}

// countryCurrency maps an ISO 3166-1 alpha-2 country to its currency, for the countries whose
// currency is supported. Euro countries map to EUR.
var countryCurrency = map[string]string{
	"KE": "KES", "US": "USD", "GB": "GBP", "UG": "UGX", "TZ": "TZS", "ZA": "ZAR", "NG": "NGN",
	"GH": "GHS", "RW": "RWF", "ET": "ETB", "EG": "EGP", "IN": "INR", "CN": "CNY", "JP": "JPY",
	"CA": "CAD", "AU": "AUD", "CH": "CHF", "AE": "AED", "SA": "SAR",
	"DE": "EUR", "FR": "EUR", "IT": "EUR", "ES": "EUR", "NL": "EUR", "BE": "EUR", "IE": "EUR",
	"PT": "EUR", "AT": "EUR", "FI": "EUR", "GR": "EUR", "LU": "EUR",
}

// IsSupportedCurrency reports whether code (any case) is in SupportedCurrencies.
func IsSupportedCurrency(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, c := range SupportedCurrencies {
		if c == code {
			return true
		}
	}
	return false
}

// CountryCurrency is the currency of a two-letter country, or empty when it is unknown or not
// supported.
func CountryCurrency(country string) string {
	return countryCurrency[strings.ToUpper(strings.TrimSpace(country))]
}

// ResolveCurrency is the tenant's currency: its own choice in metadata, else its SSO country's
// currency, else the platform default (KES when that is unset or unsupported).
func ResolveCurrency(metadata map[string]any, country *string, platformDefault string) (code, source string) {
	if v, ok := metadata[CurrencyMetadataKey].(string); ok && IsSupportedCurrency(v) {
		return strings.ToUpper(strings.TrimSpace(v)), CurrencyFromTenant
	}
	if country != nil {
		if c := CountryCurrency(*country); c != "" {
			return c, CurrencyFromCountry
		}
	}
	if IsSupportedCurrency(platformDefault) {
		return strings.ToUpper(strings.TrimSpace(platformDefault)), CurrencyFromPlatform
	}
	return "KES", CurrencyFromPlatform
}

// CurrencyOf reads the tenant and resolves its currency; a missing tenant gets the platform
// default.
func CurrencyOf(ctx context.Context, db *ent.Client, tenantID uuid.UUID, platformDefault string) string {
	t, err := db.Tenant.Get(ctx, tenantID)
	if err != nil {
		code, _ := ResolveCurrency(nil, nil, platformDefault)
		return code
	}
	code, _ := ResolveCurrency(t.Metadata, t.Country, platformDefault)
	return code
}

// ValidateCurrencySetting checks metadata.currency in a profile update: a supported code
// (normalised to upper case), or null or "" to go back to the country or platform default.
func ValidateCurrencySetting(meta map[string]any) error {
	v, ok := meta[CurrencyMetadataKey]
	if !ok || v == nil {
		return nil
	}
	s, isStr := v.(string)
	if !isStr {
		return fmt.Errorf("currency must be a currency code such as KES")
	}
	if strings.TrimSpace(s) == "" {
		meta[CurrencyMetadataKey] = nil
		return nil
	}
	if !IsSupportedCurrency(s) {
		return fmt.Errorf("currency must be one of: %s", strings.Join(SupportedCurrencies, ", "))
	}
	meta[CurrencyMetadataKey] = strings.ToUpper(strings.TrimSpace(s))
	return nil
}
