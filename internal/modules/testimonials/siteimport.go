package testimonials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Website testimonials (owner, 2026-11-24): quotes the tenant already shows on its own website are
// stored as testimonials (source website, pending review), deduplicated against the ones it has,
// and once approved feed the public page and the testimonial ad inputs like any other. Only two
// shapes are read, both verbatim: schema.org Review data (reviewBody, author, reviewRating) and
// quotes inside a page section whose class or id names testimonials or reviews (blockquote, or a
// paragraph with a cite or a name line). Nothing is summarised or reworded; text is only
// whitespace-normalised. A quote with no name is credited as the page credits it, or skipped.

// Found is one testimonial read from a page.
type Found struct {
	Author    string
	Role      string
	Body      string
	Rating    int // 0 when the page gives none
	SourceURL string
}

// Limits on what a site import reads.
const (
	maxSitePages     = 4 // the home page plus up to three testimonial or review pages
	maxSiteBytes     = 1 << 20
	maxFoundPerPage  = 30
	minFoundBodyRune = 20
)

// ExtractTestimonials reads the testimonials a page shows.
func ExtractTestimonials(page, pageURL string) []Found {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil
	}
	var out []Found
	seen := map[string]bool{}
	add := func(f Found) {
		f.Body, f.Author, f.Role = squash(f.Body), squash(f.Author), squash(f.Role)
		f.Body = strings.Trim(f.Body, "\"“”'‘’ ")
		if utf8.RuneCountInString(f.Body) < minFoundBodyRune || utf8.RuneCountInString(f.Body) > maxBodyLen || f.Author == "" ||
			utf8.RuneCountInString(f.Author) > maxNameLen || len(out) >= maxFoundPerPage {
			return
		}
		if utf8.RuneCountInString(f.Role) > maxRoleLen {
			f.Role = ""
		}
		key := BodyFingerprint(f.Body)
		if seen[key] {
			return
		}
		seen[key] = true
		f.SourceURL = pageURL
		out = append(out, f)
	}
	walk(doc, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "script" && strings.Contains(strings.ToLower(attr(n, "type")), "ld+json") {
			var v any
			if json.Unmarshal([]byte(textOf(n)), &v) == nil {
				collectReviews(v, add, 0)
			}
			return false
		}
		if n.Type == html.ElementNode && testimonialSection(n) {
			quotesIn(n, add)
			return false
		}
		return true
	})
	return out
}

func walk(n *html.Node, visit func(*html.Node) bool) {
	if !visit(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, visit)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func textOf(n *html.Node) string {
	var b strings.Builder
	walk(n, func(c *html.Node) bool {
		if c.Type == html.ElementNode && (c.Data == "script" || c.Data == "style") && c != n {
			return false
		}
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
			b.WriteByte(' ')
		}
		return true
	})
	return b.String()
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// testimonialSection is an element whose class or id names testimonials or reviews.
func testimonialSection(n *html.Node) bool {
	v := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
	return strings.Contains(v, "testimonial") || strings.Contains(v, "review")
}

// quotesIn takes each quote in a testimonials section with the name the page gives it: a
// blockquote (its cite or footer, else the next name-like line), or a figure with a figcaption.
func quotesIn(sec *html.Node, add func(Found)) {
	walk(sec, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return true
		}
		switch n.Data {
		case "figure":
			var quote, caption *html.Node
			walk(n, func(c *html.Node) bool {
				if c.Type == html.ElementNode && c.Data == "blockquote" && quote == nil {
					quote = c
				}
				if c.Type == html.ElementNode && c.Data == "figcaption" && caption == nil {
					caption = c
				}
				return true
			})
			if quote != nil && caption != nil {
				author, role := splitCredit(textOf(caption))
				add(Found{Author: author, Role: role, Body: quoteText(quote)})
				return false
			}
		case "blockquote":
			credit := ""
			walk(n, func(c *html.Node) bool {
				if c.Type == html.ElementNode && (c.Data == "cite" || c.Data == "footer") && credit == "" {
					credit = textOf(c)
				}
				return true
			})
			if credit == "" {
				credit = nextText(n)
			}
			author, role := splitCredit(credit)
			add(Found{Author: author, Role: role, Body: quoteText(n)})
			return false
		}
		return true
	})
}

// quoteText is a blockquote's words without its cite or footer.
func quoteText(q *html.Node) string {
	var b strings.Builder
	walk(q, func(c *html.Node) bool {
		if c.Type == html.ElementNode && (c.Data == "cite" || c.Data == "footer" || c.Data == "script" || c.Data == "style") {
			return false
		}
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
			b.WriteByte(' ')
		}
		return true
	})
	return b.String()
}

// nextText is the text of the next element sibling, when short enough to be a name line.
func nextText(n *html.Node) string {
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if s.Type == html.ElementNode {
			t := squash(textOf(s))
			if t != "" && utf8.RuneCountInString(t) <= 160 {
				return t
			}
			return ""
		}
	}
	return ""
}

// splitCredit reads "Jane Doe, Owner of X" or "— Jane Doe | Customer" as name and role.
func splitCredit(s string) (string, string) {
	s = strings.TrimLeft(squash(s), "-–—~ ")
	for _, sep := range []string{",", " | ", " - ", " – ", " — "} {
		if i := strings.Index(s, sep); i > 0 {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(sep):])
		}
	}
	return s, ""
}

// collectReviews walks schema.org data for Review items (top level, @graph, or a "review" list on
// a business or product).
func collectReviews(v any, add func(Found), depth int) {
	if depth > 5 {
		return
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			collectReviews(e, add, depth+1)
		}
	case map[string]any:
		if isType(x["@type"], "Review") {
			body := ldString(x["reviewBody"])
			if body == "" {
				body = ldString(x["description"])
			}
			add(Found{Author: ldName(x["author"]), Body: body, Rating: ldRating(x["reviewRating"])})
		}
		for _, k := range []string{"@graph", "review", "reviews"} {
			if c, ok := x[k]; ok {
				collectReviews(c, add, depth+1)
			}
		}
	}
}

func isType(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return strings.EqualFold(t, want)
	case []any:
		for _, e := range t {
			if isType(e, want) {
				return true
			}
		}
	}
	return false
}

func ldString(v any) string {
	s, _ := v.(string)
	return html.UnescapeString(s)
}

func ldName(v any) string {
	switch a := v.(type) {
	case string:
		return a
	case map[string]any:
		return ldString(a["name"])
	case []any:
		if len(a) > 0 {
			return ldName(a[0])
		}
	}
	return ""
}

// ldRating is a 1 to 5 rating; anything else (another scale, missing) is 0: never guessed.
func ldRating(v any) int {
	m, ok := v.(map[string]any)
	if !ok {
		return 0
	}
	best := 5.0
	if b, ok := num(m["bestRating"]); ok {
		best = b
	}
	r, ok := num(m["ratingValue"])
	if !ok || best != 5 || r < 1 || r > 5 || r != float64(int(r)) {
		return 0
	}
	return int(r)
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// testimonialLinks are same-site links whose path names testimonials or reviews.
func testimonialLinks(page string, base *url.URL) []string {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	walk(doc, func(n *html.Node) bool {
		if n.Type != html.ElementNode || n.Data != "a" || len(out) >= maxSitePages-1 {
			return true
		}
		u, err := base.Parse(attr(n, "href"))
		if err != nil || !strings.EqualFold(u.Host, base.Host) {
			return true
		}
		p := strings.ToLower(u.Path)
		if !(strings.Contains(p, "testimonial") || strings.Contains(p, "review")) {
			return true
		}
		u.Fragment, u.RawQuery = "", ""
		if s := u.String(); !seen[s] && s != base.String() {
			seen[s] = true
			out = append(out, s)
		}
		return true
	})
	return out
}

// fetchPage reads one https page with the public-only client.
func fetchPage(ctx context.Context, hc *http.Client, link string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "MarketFlowBot/1.0 (the business's own testimonials, for its review)")
	req.Header.Set("Accept", "text/html")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "html") {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSiteBytes))
	return string(b), err
}

// SiteResult is what one website import did.
type SiteResult struct {
	Pages   int
	Found   int
	Stored  int
	Already int
}

// ImportFromWebsite reads the tenant's website (home page and up to three testimonial or review
// pages on the same site) and stores each testimonial it has not got yet, pending review.
func ImportFromWebsite(ctx context.Context, db *ent.Client, hc *http.Client, tenantID uuid.UUID, website string) (SiteResult, error) {
	var res SiteResult
	u, err := url.Parse(strings.TrimSpace(website))
	if err != nil || u.Host == "" {
		return res, errors.New("website: not a link")
	}
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	if u.Scheme != "https" {
		return res, errors.New("website: https only")
	}
	home, err := fetchPage(ctx, hc, u.String())
	if err != nil {
		return res, err
	}
	pages := map[string]string{u.String(): home}
	order := []string{u.String()}
	for _, l := range testimonialLinks(home, u) {
		if p, err := fetchPage(ctx, hc, l); err == nil {
			pages[l] = p
			order = append(order, l)
		}
	}
	res.Pages = len(order)
	for _, link := range order {
		for _, f := range ExtractTestimonials(pages[link], link) {
			res.Found++
			_, err := Submit(ctx, db, SubmitInput{
				TenantID: tenantID, Source: SourceWebsite, AuthorName: f.Author, AuthorRole: f.Role, Body: f.Body,
				Rating: f.Rating, Metadata: map[string]any{"source_url": f.SourceURL, "rating_unknown": f.Rating == 0},
			})
			switch {
			case errors.Is(err, ErrDuplicate):
				res.Already++
			case err != nil:
				return res, err
			default:
				res.Stored++
			}
		}
	}
	return res, nil
}
