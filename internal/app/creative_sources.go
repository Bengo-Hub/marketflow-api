package app

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/onboarding"
)

// websiteOfferings lets the platform tenant's Creative Studio advertise what
// codevertex-website sells: Power Suite products, service lines and Digitika
// courses. The website owns them; onboarding's cached reader fetches them, and
// other tenants get nothing from this source.
func websiteOfferings(db *ent.Client, site *onboarding.WebsiteCatalog, platformSlug string) creative.ExtraSource {
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
			list []onboarding.WebsiteProduct
		}{{"PRODUCT", c.Products}, {"SERVICE", c.Services}} {
			for _, p := range group.list {
				desc := p.Tagline
				if desc == "" {
					desc = p.Description
				}
				out = append(out, creative.Offering{Source: "website", SourceID: p.ID, Type: group.kind, Name: p.Name,
					Description: desc})
			}
		}
		return out, nil
	}
}

// courseLine is a course's duration, mode and summary; the picker trims it.
func courseLine(c onboarding.WebsiteCourse) string {
	parts := []string{}
	for _, s := range []string{c.Duration, c.Mode, c.Description} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ". ")
}
