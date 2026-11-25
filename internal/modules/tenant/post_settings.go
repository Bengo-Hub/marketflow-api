package tenant

import (
	"errors"
	"net/url"
	"strings"
)

// Post and ad settings kept in tenant metadata (owner, 2026-11-25).
const (
	// PrivacyURLKey is the business's own privacy policy link (creative.PrivacyURLKey).
	PrivacyURLKey = "privacy_url"
	// PostBestTimeKey is "on" or "off": whether posts start at each platform's best time
	// (publishing.BestTimeTenantKey).
	PostBestTimeKey = "post_at_best_time"
)

// PlatformHosts are MarketFlow's own addresses (the portal and the accounts site), set at startup. A
// business's privacy policy is never one of them, unless it is the platform's own tenant.
var PlatformHosts []string

// PlatformTenantSlug is the platform owner's tenant, set at startup.
var PlatformTenantSlug string

// ValidatePostSettings checks the keys in a profile update; null or "" clears one.
func ValidatePostSettings(meta map[string]any, slug string) error {
	if v, ok := meta[PrivacyURLKey]; ok && v != nil {
		s, isStr := v.(string)
		s = strings.TrimSpace(s)
		switch {
		case !isStr:
			return errors.New("privacy_url must be a link")
		case s == "":
			meta[PrivacyURLKey] = nil
		default:
			u, err := url.Parse(s)
			if err != nil || u.Scheme != "https" || u.Host == "" || len(s) > 500 {
				return errors.New("the privacy policy link must start with https://")
			}
			if PlatformTenantSlug == "" || slug != PlatformTenantSlug {
				for _, h := range PlatformHosts {
					if h != "" && strings.EqualFold(u.Hostname(), h) {
						return errors.New("use your own business's privacy policy, not MarketFlow's")
					}
				}
			}
			meta[PrivacyURLKey] = s
		}
	}
	if v, ok := meta[PostBestTimeKey]; ok && v != nil {
		if s, isStr := v.(string); !isStr || (s != "on" && s != "off") {
			return errors.New(PostBestTimeKey + ` must be "on" or "off"`)
		}
	}
	return nil
}
