package creative

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// Where a business is and how people reach it, for the ads (owner, 2026-11-20):
//   - the setting brief places every scene in the business's own town, from its address, its
//     website pages (home, services, about, projects, blog, the offering's own page), the
//     captions of its real pictures and its market research;
//   - posters and end cards show the real contacts with icons (WhatsApp, phone, website, email
//     or the offering's own link), never a button.
//
// The address is auth-api's head office outlet (auth owns it), else the profile's business_address
// (a fallback), else the website's own (business facts), else, for a Forever Living tenant in Kenya only, the platform default
// (service-config creative.flp_default_address, Forever Living Kenya's office). The order of the
// contacts, the pages read and the default address are service-config, tenant over platform.

// Profile metadata keys for the business's address (written by the profile form).
const (
	MetaBusinessAddress = "business_address"
	MetaBusinessCity    = "business_city"
)

// Service-config keys, tenant over platform, with the documented fallbacks below.
const (
	FLPDefaultAddressKey = "creative.flp_default_address"
	CTAContactsKey       = "creative.cta_contacts"
	SettingPagesKey      = "creative.setting_pages"
)

// Fallbacks used only when the platform has not set the keys above.
const (
	// DefaultFLPAddress is Forever Living Products Kenya's office (Reinsurance Plaza, Nairobi CBD).
	DefaultFLPAddress   = "Reinsurance Plaza, Nairobi CBD"
	DefaultCTAContacts  = "whatsapp,phone,web"
	DefaultSettingPages = "/,/services,/about,/projects,/blog"
)

// placeFacts is marketflow-ai's team.PlaceFacts.
type placeFacts struct {
	Address       string      `json:"address,omitempty"`
	City          string      `json:"city,omitempty"`
	Country       string      `json:"country,omitempty"`
	AddressSource string      `json:"address_source,omitempty"`
	Website       string      `json:"website,omitempty"`
	Pages         []placePage `json:"pages,omitempty"`
	Pictures      []string    `json:"pictures,omitempty"`
	Research      string      `json:"research,omitempty"`
}

type placePage struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
	Text  string `json:"text"`
}

// contact is marketflow-ai's media.Contact.
type contact struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// FactsSource reads a tenant's business facts (marketingos.Service.BusinessFacts).
type FactsSource func(ctx context.Context, tenantID uuid.UUID) (marketingos.BusinessFacts, error)

// WithFacts lets the renderer read the business facts (website address, phone) and the website's
// pages; nil keeps the profile only.
func (r *Renderer) WithFacts(f FactsSource, pages marketingos.PageReader) *Renderer {
	r.facts, r.pages = f, pages
	return r
}

// placeInfo is what one call needs: the facts, the contacts, the brand name and the place line.
type placeInfo struct {
	Facts    placeFacts
	Contacts []contact
	Brand    string
}

// Line is the place as one line for the self-review.
func (p placeInfo) Line() string {
	parts := []string{p.Facts.Address}
	if p.Facts.City != "" && !strings.Contains(strings.ToLower(p.Facts.Address), strings.ToLower(p.Facts.City)) {
		parts = append(parts, p.Facts.City)
	}
	if p.Facts.Country != "" {
		parts = append(parts, p.Facts.Country)
	}
	return strings.Join(nonEmptyStrs(parts), ", ")
}

func nonEmptyStrs(list []string) []string {
	var out []string
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// countryNames are the tenant country codes as words for the prompts.
var countryNames = map[string]string{"KE": "Kenya", "UG": "Uganda", "TZ": "Tanzania", "RW": "Rwanda", "NG": "Nigeria", "GH": "Ghana", "ZA": "South Africa"}

// resolvePlace builds the place and contacts of a tenant. offering is the brief's offering (its
// url becomes the product link); pages reads the website's pages too (for the script only).
func (r *Renderer) resolvePlace(ctx context.Context, tenantID uuid.UUID, offering map[string]any, pages bool) placeInfo {
	t, err := r.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return placeInfo{}
	}
	out := placeInfo{Brand: t.Name}
	f := &out.Facts
	f.Address = excerpt(str(t.Metadata[MetaBusinessAddress]), 200)
	if f.Address != "" {
		f.AddressSource = "profile"
	}
	f.City = excerpt(str(t.Metadata[MetaBusinessCity]), 80)
	country := ""
	if t.Country != nil {
		country = strings.TrimSpace(*t.Country)
	}
	f.Country = countryNames[strings.ToUpper(country)]
	if f.Country == "" {
		f.Country = country
	}

	budget := 8 * time.Second
	if pages {
		budget = 30 * time.Second
	}
	facts := r.factsFor(ctx, tenantID, budget)
	// auth-api owns the address: its head office outlet wins over the MarketFlow profile, which is
	// only the fallback, and the website's own address comes after both.
	if facts.AddressSource == "account" && facts.Address != "" {
		f.Address, f.AddressSource = excerpt(facts.Address, 200), "account"
		if facts.City != "" {
			f.City = excerpt(facts.City, 80)
		}
	}
	if f.Address == "" && facts.Address != "" {
		f.Address, f.AddressSource = excerpt(facts.Address, 200), "website"
	}
	if f.Address == "" && strings.EqualFold(t.BusinessType, "flp") && (country == "" || strings.EqualFold(country, "KE")) {
		f.Address = svcconfig.Value(ctx, r.db, tenantID, FLPDefaultAddressKey)
		if f.Address == "" {
			f.Address = DefaultFLPAddress
		}
		f.AddressSource = "platform_default"
	}
	website := facts.Website
	if website == "" {
		if socials, ok := t.Metadata["socials"].(map[string]any); ok {
			website = str(socials["website"])
		}
	}
	f.Website = website
	for _, img := range brandimages.Read(t.Metadata) {
		if c := strings.TrimSpace(img.Caption); c != "" && !brandimages.LooksLikeLogo(img) {
			f.Pictures = append(f.Pictures, excerpt(c, 160))
			if len(f.Pictures) == 8 {
				break
			}
		}
	}
	if scan, ok := t.Metadata[marketingos.CompetitorScanKey].(map[string]any); ok {
		f.Research = excerpt(str(scan["summary"]), 1000)
	}
	if pages {
		f.Pages = r.readPages(ctx, tenantID, website, str(offering["url"]), facts.SiteText)
	}
	out.Contacts = contactsFor(svcconfigOr(ctx, r.db, tenantID, CTAContactsKey, DefaultCTAContacts), t, facts, str(offering["url"]), website)
	return out
}

func svcconfigOr(ctx context.Context, db *ent.Client, tenantID uuid.UUID, key, fallback string) string {
	if v := strings.TrimSpace(svcconfig.Value(ctx, db, tenantID, key)); v != "" {
		return v
	}
	return fallback
}

// contactsFor lists the contacts in the configured order, each only when the tenant has it. A
// "link" entry is the offering's own page (a product demo such as Digitika), else the website.
func contactsFor(order string, t *ent.Tenant, facts marketingos.BusinessFacts, offeringURL, website string) []contact {
	whatsapp := facts.WhatsApp
	if whatsapp == "" && t.WhatsappNumber != nil {
		whatsapp = strings.TrimSpace(*t.WhatsappNumber)
	}
	email := facts.Email
	if email == "" {
		email = str(t.Metadata["contact_email"])
	}
	values := map[string]string{"whatsapp": readablePhone(whatsapp), "phone": readablePhone(facts.Phone), "web": website, "email": email}
	if httpURL(offeringURL) {
		values["link"] = offeringURL
	} else {
		values["link"] = website
	}
	var out []contact
	seen := map[string]bool{}
	for _, k := range strings.Split(order, ",") {
		k = strings.ToLower(strings.TrimSpace(k))
		v := strings.TrimSpace(values[k])
		if v == "" || seen[v] {
			continue
		}
		// The phone is left out when it is the WhatsApp number already shown.
		if k == "phone" && digits(v) == digits(whatsapp) {
			continue
		}
		seen[v] = true
		out = append(out, contact{Kind: k, Value: excerpt(v, 60)})
		if len(out) == 4 {
			break
		}
	}
	return out
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) > 9 {
		d = d[len(d)-9:]
	}
	return d
}

func httpURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// pageTextChars bounds one page's text in the setting brief.
const pageTextChars = 900

// readPages reads the website's configured pages and the offering's own page through the
// crawler, in parallel inside one budget. The home page text the facts already hold is used as is.
func (r *Renderer) readPages(ctx context.Context, tenantID uuid.UUID, website, offeringURL, homeText string) []placePage {
	var out []placePage
	if home := strings.TrimSpace(homeText); home != "" {
		out = append(out, placePage{Title: "Home", URL: website, Text: excerpt(home, pageTextChars)})
	}
	if r.pages == nil {
		return out
	}
	var links []string
	if httpURL(offeringURL) {
		links = append(links, offeringURL)
	}
	if base, err := url.Parse(strings.TrimSpace(website)); err == nil && httpURL(website) {
		for _, p := range strings.Split(svcconfigOr(ctx, r.db, tenantID, SettingPagesKey, DefaultSettingPages), ",") {
			p = strings.TrimSpace(p)
			if p == "" || (p == "/" && len(out) > 0) {
				continue
			}
			if ref, err := url.Parse(p); err == nil {
				links = append(links, base.ResolveReference(ref).String())
			}
		}
	}
	if len(links) > 6 {
		links = links[:6]
	}
	pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	texts := make([]string, len(links))
	var wg sync.WaitGroup
	for i, l := range links {
		wg.Add(1)
		go func(i int, l string) {
			defer wg.Done()
			if text, err := r.pages(pctx, l, pageTextChars); err == nil {
				texts[i] = strings.TrimSpace(text)
			}
		}(i, l)
	}
	wg.Wait()
	for i, l := range links {
		if texts[i] == "" {
			continue
		}
		title := "Offering page"
		if !(i == 0 && httpURL(offeringURL)) {
			if u, err := url.Parse(l); err == nil {
				title = strings.Trim(u.Path, "/")
			}
		}
		out = append(out, placePage{Title: title, URL: l, Text: excerpt(texts[i], pageTextChars)})
	}
	return out
}

// factsTTL is how long a tenant's business facts are reused: the website and crawler reads take
// seconds, and a quote, a storyboard and a render of the same ad follow each other closely.
const factsTTL = 10 * time.Minute

type cachedFacts struct {
	facts marketingos.BusinessFacts
	at    time.Time
}

var (
	factsMu    sync.Mutex
	factsCache = map[uuid.UUID]cachedFacts{}
)

// factsFor reads the business facts within budget, reusing a copy read in the last factsTTL. A
// failed read is not cached and leaves the facts empty.
func (r *Renderer) factsFor(ctx context.Context, tenantID uuid.UUID, budget time.Duration) marketingos.BusinessFacts {
	if r.facts == nil {
		return marketingos.BusinessFacts{}
	}
	factsMu.Lock()
	c, ok := factsCache[tenantID]
	factsMu.Unlock()
	if ok && time.Since(c.at) < factsTTL {
		return c.facts
	}
	fctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	f, err := r.facts(fctx, tenantID)
	if err != nil {
		return marketingos.BusinessFacts{}
	}
	factsMu.Lock()
	factsCache[tenantID] = cachedFacts{facts: f, at: time.Now()}
	factsMu.Unlock()
	return f
}

// FLPDefaultAddress is the address a Forever Living tenant without its own uses: the platform's
// creative.flp_default_address, else DefaultFLPAddress.
func FLPDefaultAddress(ctx context.Context, db *ent.Client, tenantID uuid.UUID) string {
	return svcconfigOr(ctx, db, tenantID, FLPDefaultAddressKey, DefaultFLPAddress)
}

// readablePhone writes a Kenyan number the way people read it ("+254 743 793 901"); any other
// number is left as given.
func readablePhone(s string) string {
	var d strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			d.WriteRune(r)
		}
	}
	n := d.String()
	switch {
	case len(n) == 12 && strings.HasPrefix(n, "254"):
		n = n[3:]
	case len(n) == 10 && strings.HasPrefix(n, "0"):
		n = n[1:]
	default:
		return strings.TrimSpace(s)
	}
	return "+254 " + n[:3] + " " + n[3:6] + " " + n[6:]
}
