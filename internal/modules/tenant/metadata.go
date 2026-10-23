package tenant

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
)

// Business types. "flp" switches on the Forever Living sections, links and compliance rules.
const (
	BusinessTypeFLP     = "flp"
	BusinessTypeGeneric = "generic"
)

// PublicMetadataKeys are the Tenant.metadata keys the public /p/{slug} page may show.
// Everything else in metadata is private (the Marketing OS brand context, switches,
// anything a later sprint adds) and must never leave through a public route.
var PublicMetadataKeys = []string{
	"tagline",
	"bio",
	"story",
	"video_url",
	"logo_url",
	"cover_url",
	"opportunity_text",
	"socials",
	"offerings",
	"booking_enabled",
	"brand_primary_color",
	"contact_email",
}

// ReservedMetadataKeys belong to other features and are written only through their own
// endpoints. The profile PATCH ignores them, so a profile form that sends the whole
// metadata object back cannot overwrite a newer brand context.
var ReservedMetadataKeys = []string{
	marketingos.MetadataKey,
	marketingos.DailyBriefKey,
}

// PublicMetadata returns only the allow-listed keys of a tenant's metadata.
func PublicMetadata(meta map[string]any) map[string]any {
	out := make(map[string]any, len(PublicMetadataKeys))
	for _, k := range PublicMetadataKeys {
		if v, ok := meta[k]; ok {
			out[k] = v
		}
	}
	return out
}

// MergeProfileMetadata merges a profile update into the stored metadata, skipping
// reserved keys. The stored map is copied, never changed in place.
func MergeProfileMetadata(existing, update map[string]any) map[string]any {
	out := make(map[string]any, len(existing)+len(update))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range update {
		if isReserved(k) {
			continue
		}
		out[k] = v
	}
	return out
}

func isReserved(key string) bool {
	for _, r := range ReservedMetadataKeys {
		if key == r {
			return true
		}
	}
	return false
}

// Offering limits keep the public page fast and the metadata row small.
const (
	maxOfferings        = 24
	maxOfferingNameLen  = 80
	maxOfferingDescLen  = 280
	maxOfferingURLLen   = 1000
	maxOfferingLabelLen = 40
	maxOfferingCodeLen  = 12
)

// ValidateOfferings checks metadata.offerings: a list of objects with a name and
// optional description, image_url, url, price_label, flp_item_code and cc.
func ValidateOfferings(v any) error {
	if v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return fmt.Errorf("offerings must be a list")
	}
	if len(list) > maxOfferings {
		return fmt.Errorf("at most %d offerings", maxOfferings)
	}
	for i, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("offering %d must be an object", i+1)
		}
		name, _ := item["name"].(string)
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("offering %d needs a name", i+1)
		}
		if err := maxLen(item, "name", maxOfferingNameLen, i); err != nil {
			return err
		}
		if err := maxLen(item, "description", maxOfferingDescLen, i); err != nil {
			return err
		}
		if err := maxLen(item, "price_label", maxOfferingLabelLen, i); err != nil {
			return err
		}
		if err := maxLen(item, "flp_item_code", maxOfferingCodeLen, i); err != nil {
			return err
		}
		for _, k := range []string{"url", "image_url"} {
			s, _ := item[k].(string)
			if s == "" {
				continue
			}
			if len(s) > maxOfferingURLLen || !safeURL(s) {
				return fmt.Errorf("offering %d has an invalid %s", i+1, k)
			}
		}
	}
	return nil
}

func maxLen(item map[string]any, key string, limit, i int) error {
	if s, ok := item[key].(string); ok && len([]rune(s)) > limit {
		return fmt.Errorf("offering %d %s is longer than %d characters", i+1, key, limit)
	}
	return nil
}

// safeURL accepts http(s) URLs and site-relative paths ("/images/..."), never
// javascript: or data: URLs.
func safeURL(s string) bool {
	if strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//") {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}
