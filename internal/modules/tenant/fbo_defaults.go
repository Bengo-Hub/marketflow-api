package tenant

import (
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/flp"
)

// FLPLinks returns the Forever link builder for an FLP tenant, or false for any other tenant
// or an FLP tenant with no distributor ID yet. Every Forever link is formed through it.
func FLPLinks(t *ent.Tenant) (flp.LinkBuilder, bool) {
	if t == nil || !strings.EqualFold(t.BusinessType, BusinessTypeFLP) {
		return flp.LinkBuilder{}, false
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return flp.ForTenant(t.Slug, deref(t.FboDistribID), deref(t.FlpStoreCode), deref(t.FlpLanguage), deref(t.Country))
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
