package onboarding

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
)

// BrandOfferings lets every tenant advertise what it sells even without inventory or ordering
// (owner, 28 November 2026: Hadia's "What is the ad for?" picker was empty). It lists the public
// profile's offerings (tenant metadata "offerings": name, description, price label, image) and the
// brand context's "What you sell" lines, the profile first, one entry per name. The tenant owns both
// in MarketFlow, so no other service is called.
func BrandOfferings(db *ent.Client, brand BrandStore) creative.ExtraSource {
	return func(ctx context.Context, tenantID uuid.UUID) ([]creative.Offering, error) {
		t, err := db.Tenant.Get(ctx, tenantID)
		if err != nil {
			return nil, nil
		}
		seen := map[string]bool{}
		var out []creative.Offering
		add := func(o creative.Offering) {
			k := strings.ToLower(strings.TrimSpace(o.Name))
			if k == "" || seen[k] {
				return
			}
			seen[k] = true
			out = append(out, o)
		}
		list, _ := t.Metadata["offerings"].([]any)
		for i, raw := range list {
			m, _ := raw.(map[string]any)
			name, _ := m["name"].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			o := creative.Offering{Source: "profile", SourceID: "profile-" + strconv.Itoa(i), Type: "SERVICE", Name: strings.TrimSpace(name)}
			o.Description, _ = m["description"].(string)
			if label, _ := m["price_label"].(string); label != "" {
				o.Price = priceFrom(label)
				if o.Description == "" {
					o.Description = label
				}
			}
			if img, _ := m["image_url"].(string); strings.HasPrefix(img, "https://") {
				o.PhotoURLs = []string{img}
			}
			add(o)
		}
		if brand != nil {
			if bc, err := brand.GetBrandContext(ctx, tenantID); err == nil {
				for i, line := range bc.Offers {
					name, price := splitOfferLine(line)
					add(creative.Offering{Source: "brand", SourceID: "brand-" + strconv.Itoa(i), Type: "SERVICE", Name: name,
						Description: strings.TrimSpace(line), Price: price})
				}
			}
		}
		return out, nil
	}
}

var (
	// "Shea Butter Body Lotion 250ml (KES 850)" or "Gift registry: free to create".
	offerParenRe = regexp.MustCompile(`^(.*?)\s*\(([^()]*)\)\s*$`)
	amountRe     = regexp.MustCompile(`(?i)(?:kes|ksh|sh)\.?\s*([0-9][0-9,]*(?:\.[0-9]+)?)`)
)

// splitOfferLine takes a brand context offer line apart into its name and, when the line names one,
// a KES price.
func splitOfferLine(line string) (string, *float64) {
	line = strings.TrimSpace(line)
	name := line
	if m := offerParenRe.FindStringSubmatch(line); m != nil && strings.TrimSpace(m[1]) != "" {
		name = strings.TrimSpace(m[1])
	} else if i := strings.IndexAny(line, ":"); i > 0 {
		name = strings.TrimSpace(line[:i])
	}
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	return name, priceFrom(line)
}

// priceFrom reads a KES amount from free text ("KES 850", "Ksh 1,200"); nil when there is none.
func priceFrom(s string) *float64 {
	m := amountRe.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	if err != nil || v <= 0 {
		return nil
	}
	return &v
}
