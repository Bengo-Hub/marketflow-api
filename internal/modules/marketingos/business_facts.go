package marketingos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
)

// Business facts: what the chat assistant may state as true about a business (its name, how to
// reach it, where it is and when it opens), gathered from the services and pages that own them.
// The assistant is told to state an address, opening hours or phone number only from these facts
// or the knowledge base, so it never invents one (Urban Loft, 2026-11-09: it made up a location).

// AccountFacts is the business's account record (auth-api), adapted by the app.
type AccountFacts struct {
	Name, Phone, Email, Website, Storefront string
}

// AccountSource reads the account record by tenant slug.
type AccountSource func(ctx context.Context, slug string) (AccountFacts, error)

// WithAccountFacts lets BusinessFacts read the account record.
func (s *Service) WithAccountFacts(f AccountSource) *Service {
	s.accountFacts = f
	return s
}

// BusinessFacts is the block the assistant reads.
type BusinessFacts struct {
	Name     string   `json:"name"`
	Tagline  string   `json:"tagline,omitempty"`
	About    string   `json:"about,omitempty"`
	Phone    string   `json:"phone,omitempty"`
	WhatsApp string   `json:"whatsapp,omitempty"`
	Email    string   `json:"email,omitempty"`
	Website  string   `json:"website,omitempty"`
	Address  string   `json:"address,omitempty"`
	Hours    []string `json:"hours,omitempty"`
	// Summary is the website's own description of itself, which often names the town and hours.
	Summary string   `json:"summary,omitempty"`
	Sources []string `json:"sources"`
}

// BusinessFacts gathers the facts. A source that fails is left out; the tenant row always answers.
func (s *Service) BusinessFacts(ctx context.Context, tenantID uuid.UUID) (BusinessFacts, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return BusinessFacts{}, err
	}
	out := BusinessFacts{Name: t.Name, Sources: []string{"profile"}}
	if t.WhatsappNumber != nil {
		out.WhatsApp = strings.TrimSpace(*t.WhatsappNumber)
	}
	out.Tagline = metaText(t.Metadata, "tagline", 160)
	out.About = metaText(t.Metadata, "bio", 600)
	out.Email = metaText(t.Metadata, "contact_email", 120)
	if socials, ok := t.Metadata["socials"].(map[string]any); ok {
		out.Website, _ = socials["website"].(string)
	}

	if s.accountFacts != nil {
		actx, cancel := context.WithTimeout(ctx, 3*time.Second)
		a, err := s.accountFacts(actx, t.Slug)
		cancel()
		if err == nil {
			out.Sources = append(out.Sources, "account")
			if a.Name != "" {
				out.Name = a.Name
			}
			out.Phone = a.Phone
			if out.Email == "" {
				out.Email = a.Email
			}
			if out.Website == "" {
				out.Website = firstNonEmpty(a.Website, a.Storefront)
			}
		}
	}

	if out.Website != "" {
		wctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		site, err := readSite(wctx, out.Website)
		cancel()
		if err == nil {
			out.Sources = append(out.Sources, "website")
			out.Summary = site.Summary
			out.Address = site.Address
			out.Hours = site.Hours
			if out.Phone == "" {
				out.Phone = site.Phone
			}
		}
	}
	return out, nil
}

func metaText(meta map[string]any, key string, max int) string {
	v, _ := meta[key].(string)
	v = strings.Join(strings.Fields(v), " ")
	if utf8.RuneCountInString(v) > max {
		v = string([]rune(v)[:max])
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// siteFacts is what a business's own page says about itself.
type siteFacts struct {
	Summary string
	Address string
	Hours   []string
	Phone   string
}

// siteCache keeps a page's facts for a day: pages change slowly, and a chat must never wait on a
// slow site twice. Failed reads are cached too, briefly, so a dead site is not asked every time.
var siteCache = sharedcache.NewLocal[string, siteResult](2000, 24*time.Hour)

type siteResult struct {
	facts siteFacts
	err   error
	at    time.Time
}

const (
	maxPageBytes   = 768 << 10
	maxLDBytes     = 64 << 10
	failedSiteWait = 30 * time.Minute
)

var errNotPublic = errors.New("address is not public")

// siteClient fetches only public addresses: it refuses loopback, private, link-local and other
// internal ranges at connect time (after DNS, so a name cannot point it inside), and follows at most
// three redirects, each checked the same way.
var siteClient = &http.Client{
	Timeout: 6 * time.Second,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 4 * time.Second, Control: publicOnly}).DialContext,
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConns:          10,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || req.URL.Scheme != "https" {
			return errNotPublic
		}
		return nil
	},
}

func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return errNotPublic
	}
	return nil
}

// readSite reads one https page, through the day cache.
func readSite(ctx context.Context, raw string) (siteFacts, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return siteFacts{}, fmt.Errorf("website: not a link")
	}
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	if u.Scheme != "https" {
		return siteFacts{}, fmt.Errorf("website: https only")
	}
	key := u.Scheme + "://" + strings.ToLower(u.Host) + u.EscapedPath()
	if r, ok := siteCache.Get(key); ok && (r.err == nil || time.Since(r.at) < failedSiteWait) {
		return r.facts, r.err
	}
	facts, err := fetchSite(ctx, key)
	siteCache.Set(key, siteResult{facts: facts, err: err, at: time.Now()})
	return facts, err
}

func fetchSite(ctx context.Context, link string) (siteFacts, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return siteFacts{}, err
	}
	req.Header.Set("User-Agent", "MarketFlowBot/1.0 (business facts for the business's own assistant)")
	req.Header.Set("Accept", "text/html")
	resp, err := siteClient.Do(req)
	if err != nil {
		return siteFacts{}, fmt.Errorf("website: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "html") {
		return siteFacts{}, fmt.Errorf("website: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return siteFacts{}, err
	}
	return parseSite(string(body)), nil
}

var (
	titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.{1,300}?)</title>`)
	metaRe  = regexp.MustCompile(`(?is)<meta\s[^>]*>`)
	attrRe  = regexp.MustCompile(`(?is)(name|property|content)\s*=\s*"([^"]*)"`)
	// RE2 caps repeat counts at 1000, so the block is unbounded here; the page itself is capped
	// at maxPageBytes and blocks over maxLDBytes are skipped.
	ldJSONRe  = regexp.MustCompile(`(?is)<script[^>]*type\s*=\s*"application/ld\+json"[^>]*>(.*?)</script>`)
	spacesRe  = regexp.MustCompile(`\s+`)
	maxHours  = 7
	maxDetail = 300
)

// parseSite takes the page's own description (title and meta description) and, when the page
// publishes it, schema.org business data: address, opening hours and phone. Nothing else is read.
func parseSite(page string) siteFacts {
	var out siteFacts
	title := ""
	if m := titleRe.FindStringSubmatch(page); m != nil {
		title = clean(m[1])
	}
	desc := ""
	for _, tag := range metaRe.FindAllString(page, 40) {
		attrs := map[string]string{}
		for _, a := range attrRe.FindAllStringSubmatch(tag, -1) {
			attrs[strings.ToLower(a[1])] = a[2]
		}
		if n := strings.ToLower(firstNonEmpty(attrs["name"], attrs["property"])); (n == "description" || n == "og:description") && desc == "" {
			desc = clean(attrs["content"])
		}
	}
	out.Summary = strings.Trim(strings.Join(nonEmpty(title, desc), ". "), " .")
	if utf8.RuneCountInString(out.Summary) > maxDetail {
		out.Summary = string([]rune(out.Summary)[:maxDetail])
	}
	for _, m := range ldJSONRe.FindAllStringSubmatch(page, 5) {
		if len(m[1]) > maxLDBytes {
			continue
		}
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v) != nil {
			continue
		}
		collectLD(v, &out, 0)
	}
	return out
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func clean(s string) string {
	return strings.TrimSpace(spacesRe.ReplaceAllString(html.UnescapeString(s), " "))
}

// collectLD walks schema.org data (objects, lists and @graph) for the first address, phone and
// opening hours, at a bounded depth.
func collectLD(v any, out *siteFacts, depth int) {
	if depth > 4 {
		return
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			collectLD(e, out, depth+1)
		}
	case map[string]any:
		if out.Address == "" {
			out.Address = ldAddress(x["address"])
		}
		if out.Phone == "" {
			if p, ok := x["telephone"].(string); ok {
				out.Phone = clean(p)
			}
		}
		if len(out.Hours) == 0 {
			out.Hours = ldHours(x)
		}
		if g, ok := x["@graph"]; ok {
			collectLD(g, out, depth+1)
		}
	}
}

func ldAddress(v any) string {
	switch a := v.(type) {
	case string:
		return clean(a)
	case map[string]any:
		var parts []string
		for _, k := range []string{"streetAddress", "addressLocality", "addressRegion", "addressCountry"} {
			if s, ok := a[k].(string); ok && clean(s) != "" {
				parts = append(parts, clean(s))
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func ldHours(x map[string]any) []string {
	var out []string
	switch h := x["openingHours"].(type) {
	case string:
		out = append(out, clean(h))
	case []any:
		for _, e := range h {
			if s, ok := e.(string); ok {
				out = append(out, clean(s))
			}
		}
	}
	if specs, ok := x["openingHoursSpecification"].([]any); ok {
		for _, e := range specs {
			m, _ := e.(map[string]any)
			opens, _ := m["opens"].(string)
			closes, _ := m["closes"].(string)
			if opens == "" || closes == "" {
				continue
			}
			days := ""
			switch d := m["dayOfWeek"].(type) {
			case string:
				days = d
			case []any:
				var ds []string
				for _, v := range d {
					if s, ok := v.(string); ok {
						ds = append(ds, strings.TrimPrefix(s, "https://schema.org/"))
					}
				}
				days = strings.Join(ds, ", ")
			}
			out = append(out, strings.TrimSpace(clean(days)+" "+opens+" to "+closes))
		}
	}
	if len(out) > maxHours {
		out = out[:maxHours]
	}
	return out
}
