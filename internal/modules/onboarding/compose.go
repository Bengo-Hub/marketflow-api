package onboarding

import (
	"fmt"
	"math"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
)

type composeInput struct {
	tenant       *ent.Tenant
	current      marketingos.BrandContext
	auth         *tenant.AuthProfile
	top          []creative.TopOffering
	catalog      Catalog
	website      bool
	testimonials []*ent.Testimonial
}

// Claims every business must avoid (Consumer Protection Act 2012), and the Forever Living
// rules from docs/compliance-rules.md for FBO tenants.
var (
	genericBannedClaims = []string{
		"False or misleading claims about price, quality or results",
	}
	flpBannedClaims = []string{
		"Saying a product cures, treats, heals or prevents any disease",
		"Income promises: guaranteed income, get rich, passive income, financial freedom",
		"Income figures or screenshots of earnings",
		"Calling products FDA approved, a miracle or a wonder drug",
		"Using Forever Living logos without authorisation",
	}
)

// itemLen matches marketingos' per-item limit, so a draft always saves.
const itemLen = 300

// compose builds the draft without any I/O, so it is easy to test.
func compose(in composeInput) Draft {
	meta := in.tenant.Metadata
	d := Draft{Current: in.current, Sources: []string{"profile"}}
	isFLP := in.tenant.BusinessType == tenant.BusinessTypeFLP

	// Offerings to link: best sellers for ordinary tenants, the website for the platform tenant.
	if len(in.top) > 0 {
		d.Sources = append(d.Sources, "catalogue")
		for _, o := range in.top {
			s := Suggestion{Name: clip(o.Name, 80), Description: clip(o.Description, 280), Source: "inventory", QuantitySold: o.QuantitySold}
			if o.QuantitySold > 0 {
				s.Source = "pos"
			}
			if o.Price != nil && *o.Price > 0 {
				s.PriceLabel = kes(*o.Price)
			}
			if len(o.PhotoURLs) > 0 {
				s.ImageURL = o.PhotoURLs[0]
			}
			d.Offerings = append(d.Offerings, s)
		}
	}
	if in.website {
		d.Sources = append(d.Sources, "website")
		for _, p := range in.catalog.Products {
			d.Offerings = append(d.Offerings, Suggestion{Name: clip(p.Name, 80), Description: clip(p.Description, 280), URL: p.URL, Source: "website"})
		}
		for _, c := range in.catalog.Courses {
			s := Suggestion{Name: clip(c.Name, 80), Description: clip(c.Description, 280), Source: "course", ImageURL: absoluteOrEmpty(c.CoverImage)}
			if c.Price > 0 {
				s.PriceLabel = kes(float64(c.Price))
			}
			d.Offerings = append(d.Offerings, s)
		}
	}
	if len(d.Offerings) > 20 {
		d.Offerings = d.Offerings[:20]
	}

	// Profile defaults from auth-api, only where MarketFlow has nothing yet.
	if a := in.auth; a != nil {
		d.Sources = append(d.Sources, "account")
		if str(meta, "contact_email") == "" {
			d.Profile.ContactEmail = a.ContactEmail
		}
		if in.tenant.WhatsappNumber == nil || strings.TrimSpace(*in.tenant.WhatsappNumber) == "" {
			d.Profile.WhatsApp = a.ContactPhone
		}
		if str(meta, "logo_url") == "" {
			d.Profile.LogoURL = a.LogoURL
		}
		d.Profile.Website = a.Website
		if str(meta, "brand_primary_color") == "" {
			d.Profile.BrandColor = a.PrimaryColor
		}
	}

	// The suggested context keeps every field the owner filled and fills the empty ones.
	s := in.current
	if strings.TrimSpace(s.Positioning) == "" {
		s.Positioning = firstNonEmpty(str(meta, "bio"), str(meta, "tagline"))
		if s.Positioning == "" && in.website && len(in.catalog.Products) > 0 {
			names := make([]string, 0, 4)
			for _, p := range in.catalog.Products {
				if len(names) == 4 {
					break
				}
				names = append(names, p.Name)
			}
			s.Positioning = clip(fmt.Sprintf("%s builds business software for African companies, including %s.", in.tenant.Name, strings.Join(names, ", ")), 1200)
		}
	}
	if len(s.Offers) == 0 {
		for _, o := range d.Offerings {
			line := o.Name
			if o.PriceLabel != "" {
				line += " (" + o.PriceLabel + ")"
			}
			s.Offers = append(s.Offers, clip(line, itemLen))
		}
	}
	if len(s.Proof) == 0 {
		for _, t := range in.testimonials {
			quote := fmt.Sprintf("“%s” (%s)", strings.TrimSpace(t.Body), t.AuthorName)
			s.Proof = append(s.Proof, clip(quote, itemLen))
		}
		if in.website {
			for _, p := range in.catalog.Products {
				if len(p.Highlights) > 0 && len(s.Proof) < 20 {
					s.Proof = append(s.Proof, clip(p.Name+": "+strings.Join(p.Highlights, "; "), itemLen))
				}
			}
		}
	}
	if len(s.BannedClaims) == 0 {
		s.BannedClaims = append(s.BannedClaims, genericBannedClaims...)
		if isFLP {
			s.BannedClaims = append(s.BannedClaims, flpBannedClaims...)
		}
	}
	s.Offers = cap20(s.Offers)
	s.Proof = cap20(s.Proof)
	d.Suggested = s
	return d
}

func kes(v float64) string {
	n := int64(math.Round(v))
	digits := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return "KES " + b.String()
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func cap20(l []string) []string {
	if len(l) > 20 {
		return l[:20]
	}
	return l
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func absoluteOrEmpty(u string) string {
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return u
	}
	return ""
}
