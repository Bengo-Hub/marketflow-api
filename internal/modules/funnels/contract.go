package funnels

import (
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Funnel copy and theme live in Funnel.metadata under these keys (no schema fields).
const (
	MetaHeadline    = "headline"
	MetaSubheadline = "subheadline"
	MetaDescription = "description"
	MetaBrandColor  = "brand_color"
	MetaHeroImage   = "hero_image_url"
)

// legacyFields is the older request shape still sent by marketflow-ai's create_funnel tool:
// steps and theme nested in content_json, and the copy as top-level fields. Before 2026-10-23
// the API ignored all of it, so funnels built by the assistant were saved with no steps or copy.
type legacyFields struct {
	ContentJSON *struct {
		Steps []map[string]any `json:"steps"`
		Theme map[string]any   `json:"theme"`
	} `json:"content_json"`
	Headline    *string `json:"headline"`
	Subheadline *string `json:"subheadline"`
	Description *string `json:"description"`
}

// fold moves the legacy fields into steps and metadata. Values already given in the canonical
// fields win.
func (l legacyFields) fold(steps []map[string]any, meta map[string]any) ([]map[string]any, map[string]any) {
	if l.ContentJSON != nil && steps == nil && l.ContentJSON.Steps != nil {
		steps = l.ContentJSON.Steps
	}
	set := func(key string, v any) {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return
		}
		if meta == nil {
			meta = map[string]any{}
		}
		if _, exists := meta[key]; !exists {
			meta[key] = strings.TrimSpace(s)
		}
	}
	if l.Headline != nil {
		set(MetaHeadline, *l.Headline)
	}
	if l.Subheadline != nil {
		set(MetaSubheadline, *l.Subheadline)
	}
	if l.Description != nil {
		set(MetaDescription, *l.Description)
	}
	if l.ContentJSON != nil {
		set(MetaBrandColor, l.ContentJSON.Theme[MetaBrandColor])
		set(MetaHeroImage, l.ContentJSON.Theme[MetaHeroImage])
	}
	return steps, meta
}

// PublicFunnel is what GET /public/funnels/{slug} returns: what the page needs to render and
// nothing else (no tenant id, counters or internal metadata).
type PublicFunnel struct {
	Name            string           `json:"name"`
	Slug            string           `json:"slug"`
	FunnelType      string           `json:"funnel_type"`
	TenantSlug      string           `json:"tenant_slug"`
	Headline        string           `json:"headline,omitempty"`
	Subheadline     string           `json:"subheadline,omitempty"`
	Description     string           `json:"description,omitempty"`
	BrandColor      string           `json:"brand_color,omitempty"`
	HeroImageURL    string           `json:"hero_image_url,omitempty"`
	Steps           []map[string]any `json:"steps"`
	LandingPageHTML string           `json:"landing_page_html,omitempty"`
}

// ToPublic builds the public view of a published funnel. The HTML is sanitised again here so
// rows saved before the save-time check are safe too.
func ToPublic(f *ent.Funnel, tenantSlug string) PublicFunnel {
	str := func(k string) string {
		s, _ := f.Metadata[k].(string)
		return strings.TrimSpace(s)
	}
	steps := f.Steps
	if steps == nil {
		steps = []map[string]any{}
	}
	out := PublicFunnel{
		Name:            f.Name,
		Slug:            f.Slug,
		FunnelType:      f.FunnelType,
		TenantSlug:      tenantSlug,
		Headline:        str(MetaHeadline),
		Subheadline:     str(MetaSubheadline),
		Description:     str(MetaDescription),
		BrandColor:      SafeColor(str(MetaBrandColor), ""),
		Steps:           steps,
		LandingPageHTML: SanitizeLandingHTML(f.LandingPageHTML),
	}
	if u := str(MetaHeroImage); strings.HasPrefix(u, "https://") {
		out.HeroImageURL = u
	}
	return out
}
