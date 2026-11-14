package onboarding

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
)

// WebsiteOfferings lets the platform tenant's Creative Studio advertise what
// codevertex-website sells: Power Suite products, service lines and Digitika
// courses. The website owns them; onboarding's cached reader fetches them, and
// other tenants get nothing from this source.
func WebsiteOfferings(db *ent.Client, site *WebsiteCatalog, platformSlug string) creative.ExtraSource {
	if site == nil || platformSlug == "" {
		return nil
	}
	return func(ctx context.Context, tenantID uuid.UUID) ([]creative.Offering, error) {
		t, err := db.Tenant.Get(ctx, tenantID)
		if err != nil || t.Slug != platformSlug {
			return nil, nil
		}
		c, err := site.Load(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]creative.Offering, 0, len(c.Courses)+len(c.Products)+len(c.Services))
		for _, co := range c.Courses {
			off := creative.Offering{Source: "website", SourceID: co.Slug, Type: "COURSE", Name: co.Name,
				Description: courseLine(co)}
			if co.Price > 0 {
				price := float64(co.Price)
				off.Price = &price
			}
			if strings.HasPrefix(co.CoverImage, "https://") || strings.HasPrefix(co.CoverImage, "http://") {
				off.PhotoURLs = []string{co.CoverImage}
			}
			out = append(out, off)
		}
		for _, group := range []struct {
			kind string
			list []WebsiteProduct
		}{{"PRODUCT", c.Products}, {"SERVICE", c.Services}} {
			for _, p := range group.list {
				out = append(out, creative.Offering{Source: "website", SourceID: p.ID, Type: group.kind, Name: p.Name,
					Description: productLine(p)})
			}
		}
		return out, nil
	}
}

// productLine describes a Power Suite product or service line for the Studio and the platform
// tenant's knowledge base (Sprint 7 #36): the summary, up to four highlights and where to sign up,
// so Vera answers "where do I get the POS?" with the product's own link.
func productLine(p WebsiteProduct) string {
	desc := strings.TrimSpace(p.Tagline)
	if desc == "" {
		desc = strings.TrimSpace(p.Description)
	}
	parts := []string{}
	if desc != "" {
		parts = append(parts, strings.TrimRight(desc, "."))
	}
	points := p.Highlights
	if len(points) == 0 {
		points = p.Features
	}
	if len(points) > 4 {
		points = points[:4]
	}
	if len(points) > 0 {
		parts = append(parts, strings.Join(points, ", "))
	}
	if u := strings.TrimSpace(p.URL); strings.HasPrefix(u, "https://") {
		parts = append(parts, "Get it at "+u)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ". ") + "."
}

// courseLine is a course's duration, mode and summary; the picker trims it.
func courseLine(c WebsiteCourse) string {
	parts := []string{}
	for _, s := range []string{c.Duration, c.Mode, c.Description} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ". ")
}
