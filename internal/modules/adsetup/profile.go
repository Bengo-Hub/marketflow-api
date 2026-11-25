package adsetup

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/platform/shareddefaults"
)

// profileTimeout bounds the website read; the checklist works without it.
const profileTimeout = 8 * time.Second

// loadProfile gathers the business's own facts for the proposed asset texts. Every source is
// optional: a missing one leaves its rows empty for the owner to fill in.
func loadProfile(ctx context.Context, t *ent.Tenant, facts func(context.Context) (marketingos.BusinessFacts, error), hc *http.Client) Profile {
	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()
	p := Profile{Name: t.Name}
	if t.Country != nil {
		p.Country = strings.ToUpper(*t.Country)
	}
	offers, _ := shareddefaults.Meta(t)["offerings"].([]any)
	var offerPages []Page
	for _, o := range offers {
		m, _ := o.(map[string]any)
		name, _ := m["name"].(string)
		u, _ := m["url"].(string)
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		p.Services = append(p.Services, name)
		offerPages = append(offerPages, Page{Title: name, URL: u})
		if d, _ := m["description"].(string); strings.TrimSpace(d) != "" {
			p.Descriptions = append(p.Descriptions, strings.TrimSpace(d))
		}
	}
	if facts != nil {
		if f, err := facts(ctx); err == nil {
			p.Website, p.Phone, p.City = f.Website, f.Phone, f.City
			if p.Phone == "" {
				p.Phone = f.WhatsApp
			}
		}
	}
	if p.Website != "" && !strings.Contains(p.Website, "://") {
		p.Website = "https://" + p.Website
	}
	host := hostOf(p.Website)
	for _, pg := range offerPages {
		if httpsURL(pg.URL) && hostOf(pg.URL) == host {
			p.Pages = append(p.Pages, pg)
		}
	}
	if host != "" && hc != nil {
		p.Pages = append(p.Pages, sitePages(ctx, hc, p.Website)...)
	}
	p.Pages = distinctPages(p.Pages)
	return p
}

var (
	anchorRE = regexp.MustCompile(`(?is)<a\s[^>]*?href\s*=\s*["']([^"']+)["'][^>]*>(.*?)</a>`)
	tagRE    = regexp.MustCompile(`(?s)<[^>]*>`)
	// Pages that make no sense under an ad.
	skipRE = regexp.MustCompile(`(?i)(login|sign-?in|sign-?up|register|cart|checkout|account|privacy|terms|cookie|\.(pdf|jpe?g|png|zip)$)`)
)

// sitePages reads the home page's own links.
func sitePages(ctx context.Context, hc *http.Client, site string) []Page {
	return linksIn(site, pageBody(ctx, hc, site))
}

// pageBody is a public page's HTML, "" when it does not open.
func pageBody(ctx context.Context, hc *http.Client, page string) string {
	resp := getPage(ctx, hc, page)
	if resp == nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(body)
}

// opens reports whether a public page answers.
func opens(ctx context.Context, hc *http.Client, page string) bool {
	resp := getPage(ctx, hc, page)
	if resp == nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func getPage(ctx context.Context, hc *http.Client, page string) *http.Response {
	if hc == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return nil
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil
	}
	return resp
}

func linksIn(site, body string) []Page {
	base, err := url.Parse(site)
	if err != nil {
		return nil
	}
	var out []Page
	for _, m := range anchorRE.FindAllStringSubmatch(body, 200) {
		ref, err := url.Parse(strings.TrimSpace(html.UnescapeString(m[1])))
		if err != nil {
			continue
		}
		u := base.ResolveReference(ref)
		u.Fragment, u.RawQuery = "", ""
		title := strings.Join(strings.Fields(html.UnescapeString(tagRE.ReplaceAllString(m[2], " "))), " ")
		if u.Scheme != "https" || hostOf(u.String()) != hostOf(site) || strings.Trim(u.Path, "/") == "" ||
			skipRE.MatchString(u.Path) || title == "" || len([]rune(title)) > maxSitelinkText {
			continue
		}
		out = append(out, Page{Title: title, URL: u.String()})
	}
	return out
}

// distinctPages keeps one page per address and per text; Google refuses sitelinks that repeat.
func distinctPages(in []Page) []Page {
	seen := map[string]bool{}
	var out []Page
	for _, p := range in {
		u, t := strings.TrimSuffix(strings.ToLower(p.URL), "/"), strings.ToLower(p.Title)
		if seen[u] || seen["t:"+t] {
			continue
		}
		seen[u], seen["t:"+t] = true, true
		out = append(out, p)
	}
	return out
}
