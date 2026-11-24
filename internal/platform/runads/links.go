package runads

import (
	"net/url"
	"strings"
)

// TikTokLink fills a TikTok Ads Manager page with the advertiser id; without an advertiser the
// sign-up page is the only useful link.
func (s Settings) TikTokLink(page, advertiserID string) string {
	tpl := s.TikTokLinks[page]
	if tpl == "" {
		tpl = DefaultTikTokLinks()[page]
	}
	if strings.Contains(tpl, "{advertiser_id}") {
		if strings.TrimSpace(advertiserID) == "" {
			return s.TikTokLinkOr("signup")
		}
		tpl = strings.ReplaceAll(tpl, "{advertiser_id}", url.QueryEscape(strings.TrimSpace(advertiserID)))
	}
	return tpl
}

// TikTokLinkOr is a page that takes no advertiser id.
func (s Settings) TikTokLinkOr(page string) string {
	if u := s.TikTokLinks[page]; u != "" {
		return u
	}
	return DefaultTikTokLinks()[page]
}

// GoogleBillingLink is the billing page for a customer; the id goes in only where the template
// asks for it.
func (s Settings) GoogleBillingLink(customerID string) string {
	tpl := s.GoogleBilling
	if tpl == "" {
		tpl = DefaultGoogleBillingURL
	}
	digits := strings.ReplaceAll(strings.TrimSpace(customerID), "-", "")
	return strings.ReplaceAll(tpl, "{customer_id}", url.QueryEscape(digits))
}
