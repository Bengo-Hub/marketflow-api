package brandimages

import (
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// Choosing the pictures of a website that show the business itself (owner, 2026-11-19). A site is
// read as a page, not as a bag of image tags: a software house such as Codevertex shows its
// clients' logos in a "Trusted by" marquee, and those logos belong to the clients, never to the
// site's owner. A picture is left out when it sits in a part of the page named for clients,
// partners, sponsors or logos, in the navigation or the footer, when it is shown small (an icon or
// a logo), when its file or description says logo, when it repeats like a looping strip, or when
// its description names another business on MarketFlow. What is left is ranked: pictures in the
// hero, services, about, products, projects, blog and call to action parts come first.

// Pick is one picture chosen from a page, with where it was found.
type Pick struct {
	URL string `json:"url"`
	// Page is the address of the page the picture was found on.
	Page string `json:"page"`
	// Section names the part of the page ("hero", "services", "share image"), empty when unknown.
	Section string `json:"section,omitempty"`
	Alt     string `json:"alt,omitempty"`
	score   int
	order   int
	key     string
}

// Names are the business names a picture is checked against: the tenant's own (its logo is a
// profile picture, not a brand photo) and every other business on MarketFlow (never taken).
type Names struct {
	Own    string
	Others []string
}

// nameWords are left out when names are compared, so "THE URBAN LOFT CAFE LIMITED" still matches
// "The Urban Loft".
var nameWords = map[string]bool{"the": true, "limited": true, "ltd": true, "llc": true, "inc": true,
	"plc": true, "co": true, "company": true, "enterprises": true, "enterprise": true, "group": true, "and": true}

// words splits text into lower case words, breaking camelCase and anything that is not a letter or
// a digit ("clientLogos", "customers-heading", "logo-full.png").
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// hasWord reports the first of list found among ws, counting a plain plural ("clients") as the
// word. Empty when none is.
func hasWord(ws []string, list []string) string {
	for _, w := range ws {
		for _, l := range list {
			if w == l || w == l+"s" || w+"s" == l {
				return l
			}
		}
	}
	return ""
}

// nameTokens is a business name as the words that tell it apart.
func nameTokens(name string) []string {
	var out []string
	for _, w := range words(name) {
		if !nameWords[w] {
			out = append(out, w)
		}
	}
	if len(strings.Join(out, "")) < 4 {
		return nil // too short to tell apart from ordinary words
	}
	return out
}

// containsRun reports whether ws holds the words of name one after another.
func containsRun(ws, name []string) bool {
	if len(name) == 0 || len(name) > len(ws) {
		return false
	}
	for i := 0; i+len(name) <= len(ws); i++ {
		ok := true
		for j := range name {
			if ws[i+j] != name[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return strings.TrimSpace(a.Val)
		}
	}
	return ""
}

func textOf(n *html.Node, limit int) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if b.Len() >= limit {
			return
		}
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
			b.WriteByte(' ')
		}
		if c.Type == html.ElementNode && (c.Data == "script" || c.Data == "style") {
			return
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// firstHeading is the text of the first h1 to h4 inside n.
func firstHeading(n *html.Node) string {
	var found string
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if found != "" {
			return
		}
		if c.Type == html.ElementNode {
			switch c.Data {
			case "h1", "h2", "h3", "h4":
				found = textOf(c, 160)
				return
			}
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return found
}

var sectionTags = map[string]bool{"section": true, "article": true, "aside": true, "header": true}

// reTinyClass finds utility classes that fix a small display size: Tailwind h-8, w-9, size-12.
var reTinyClass = regexp.MustCompile(`(?:^|\s)(?:[a-z]+:)?(?:h|w|size|max-h|max-w)-(\d+(?:\.\d+)?)(?:\s|$)`)

// pageParts is what the first pass learns about a page.
type pageParts struct {
	idText   map[string]string // text of elements by id, for aria-labelledby
	srcCount map[string]int    // how often each picture appears
	share    []string          // og:image, twitter:image, image_src
	ld       []string          // structured data images
	links    []string          // href of every link
}

func scanPage(doc *html.Node) pageParts {
	p := pageParts{idText: map[string]string{}, srcCount: map[string]int{}}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if id := attr(n, "id"); id != "" {
				p.idText[id] = textOf(n, 160)
			}
			switch n.Data {
			case "meta":
				key := strings.ToLower(attr(n, "property") + attr(n, "name"))
				if key == "og:image" || key == "og:image:url" || key == "og:image:secure_url" || key == "twitter:image" {
					p.share = append(p.share, attr(n, "content"))
				}
			case "link":
				if strings.EqualFold(attr(n, "rel"), "image_src") {
					p.share = append(p.share, attr(n, "href"))
				}
			case "a":
				if h := attr(n, "href"); h != "" {
					p.links = append(p.links, h)
				}
			case "script":
				if strings.Contains(attr(n, "type"), "ld+json") && n.FirstChild != nil {
					for _, m := range reLDImage.FindAllStringSubmatch(n.FirstChild.Data, -1) {
						for _, u := range reURLInJSON.FindAllStringSubmatch(m[1], -1) {
							p.ld = append(p.ld, strings.ReplaceAll(u[1], `\/`, "/"))
						}
					}
				}
			case "img":
				if s := imgSource(n); s != "" {
					p.srcCount[s]++
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return p
}

// imgSource is the picture an img or source element shows: its widest srcset entry, else its src.
func imgSource(n *html.Node) string {
	if best := largestSrcset(attr(n, "srcset")); best != "" {
		return best
	}
	if best := largestSrcset(attr(n, "data-srcset")); best != "" {
		return best
	}
	for _, k := range []string{"src", "data-src", "data-lazy-src"} {
		if v := attr(n, k); v != "" {
			return v
		}
	}
	return ""
}

// widestSrcset is the largest width a srcset offers (0 when it gives none in w).
func widestSrcset(srcset string) int {
	best := 0
	for _, part := range strings.Split(srcset, ",") {
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) > 1 && strings.HasSuffix(f[1], "w") {
			if n, err := strconv.Atoi(strings.TrimSuffix(f[1], "w")); err == nil && n > best {
				best = n
			}
		}
	}
	return best
}

// innerPicture is the picture an image proxy serves (Next.js /_next/image?url=..., most CDNs
// that take ?url=), so the real file name is checked, not "image".
func innerPicture(u *url.URL) string {
	if inner := u.Query().Get("url"); inner != "" {
		if iu, err := url.Parse(inner); err == nil {
			return iu.Path
		}
	}
	return u.Path
}

// PagePictures returns the pictures of one page that show the business, best first, and how many
// were left out for each reason.
func PagePictures(page, pageURL string, r SiteRules, names Names) ([]Pick, map[string]int) {
	skipped := map[string]int{}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, skipped
	}
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil, skipped
	}
	parts := scanPage(doc)
	own := nameTokens(names.Own)
	var others [][]string
	for _, n := range names.Others {
		if t := nameTokens(n); t != nil && !(own != nil && strings.Join(t, " ") == strings.Join(own, " ")) {
			others = append(others, t)
		}
	}

	var picks []Pick
	order := 0
	// judge decides on one picture address with what is known about where it sits.
	judge := func(raw, alt, section string, bonus int, tiny bool) {
		order++
		u := normaliseHTTPS(raw, base)
		if u == "" {
			return
		}
		pu, _ := url.Parse(u)
		inner := strings.ToLower(innerPicture(pu))
		for _, ext := range []string{".svg", ".gif", ".ico", ".avif", ".bmp"} {
			if strings.HasSuffix(inner, ext) {
				skipped["not_a_photo"]++
				return
			}
		}
		fileWords := words(inner)
		altWords := words(alt)
		switch {
		case hasWord(fileWords, r.LogoWords) != "" || hasWord(altWords, r.LogoWords) != "":
			skipped["logo"]++
			return
		case own != nil && strings.Join(altWords, " ") != "" && sameName(altWords, own):
			skipped["logo"]++ // a picture described only by the business's name is its logo
			return
		case tiny:
			skipped["too_small"]++
			return
		}
		for _, o := range others {
			if containsRun(altWords, o) || containsRun(fileWords, o) {
				skipped["other_business"]++
				return
			}
		}
		score := 50 + bonus
		if len(altWords) >= 3 {
			score += 10 // described like a photo, not a name
		}
		key := inner
		if pu.Host != "" {
			key = pu.Host + inner
		}
		picks = append(picks, Pick{URL: u, Page: pageURL, Section: section, Alt: alt, score: score, order: order, key: key})
	}

	for _, s := range parts.share {
		judge(s, "", "share image", 15, false)
	}
	for _, s := range parts.ld {
		judge(s, "", "business details", 10, false)
	}

	sectionNo := -1
	var walk func(n *html.Node, stack []*html.Node)
	walk = func(n *html.Node, stack []*html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "section" {
				sectionNo++
			}
			if n.Data == "img" || (n.Data == "source" && len(stack) > 0 && stack[len(stack)-1].Data == "picture") {
				considerImg(n, stack, sectionNo, parts, r, skipped, judge)
				return
			}
			stack = append(stack, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, stack)
		}
	}
	walk(doc, nil)
	return rank(picks), skipped
}

func sameName(alt, own []string) bool {
	var a []string
	for _, w := range alt {
		if !nameWords[w] {
			a = append(a, w)
		}
	}
	return strings.Join(a, " ") == strings.Join(own, " ")
}

// considerImg looks at where an img sits and hands it to judge, or counts why it was left out.
func considerImg(n *html.Node, stack []*html.Node, sectionNo int, parts pageParts, r SiteRules,
	skipped map[string]int, judge func(raw, alt, section string, bonus int, tiny bool)) {
	src := imgSource(n)
	if src == "" {
		return
	}
	// A picture repeated three times or more is a looping strip (a marquee shows its list twice
	// or three times so the loop has no gap).
	if parts.srcCount[src] >= 3 {
		skipped["logo_strip"]++
		return
	}
	widest := max(widestSrcset(attr(n, "srcset")), widestSrcset(attr(n, "data-srcset")))
	w, _ := strconv.Atoi(attr(n, "width"))
	h, _ := strconv.Atoi(attr(n, "height"))
	// A large photo is not left out for a heading alone: a hero titled "Trusted by 500 shops" still
	// shows the business. Names a designer gave the part (id, class, label) always count.
	large := widest >= 800 || max(w, h) >= 2*r.MinSide
	if hasWord(words(attr(n, "id")+" "+attr(n, "class")+" "+attr(n, "title")), r.ExcludeWords) != "" {
		skipped["not_the_business"]++ // the picture itself is named a client or partner item
		return
	}
	section := ""
	for i := len(stack) - 1; i >= 0; i-- {
		a := stack[i]
		switch a.Data {
		case "html", "body", "main":
			continue
		case "nav", "footer", "marquee":
			skipped["page_chrome"]++
			return
		}
		switch strings.ToLower(attr(a, "role")) {
		case "navigation", "contentinfo":
			skipped["page_chrome"]++
			return
		}
		named := words(attr(a, "id") + " " + attr(a, "class") + " " + attr(a, "aria-label"))
		heading := ""
		if ref := attr(a, "aria-labelledby"); ref != "" {
			for _, id := range strings.Fields(ref) {
				heading += " " + parts.idText[id]
			}
		} else if sectionTags[a.Data] {
			heading = firstHeading(a)
		}
		headed := words(heading)
		if hasWord(named, r.ExcludeWords) != "" || (!large && hasWord(headed, r.ExcludeWords) != "") {
			skipped["not_the_business"]++
			return
		}
		if section == "" {
			if section = hasWord(named, r.PreferWords); section == "" {
				section = hasWord(headed, r.PreferWords)
			}
		}
	}
	bonus := 0
	if section != "" {
		bonus += 30
	}
	if section == "" && sectionNo == 0 {
		section, bonus = "hero", bonus+20 // the first part of a page is its hero
	}
	if section == "" && sectionNo < 0 {
		section, bonus = "hero", bonus+10 // before any section: a header picture
	}

	// Shown small: declared width and height, or a utility class like h-9, with no large version.
	class := attr(n, "class")
	tiny := false
	if w > 0 && h > 0 && max(w, h) < r.MinSide && widest < r.MinSide {
		tiny = true
	}
	for _, m := range reTinyClass.FindAllStringSubmatch(" "+class+" ", -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v*4 < 100 {
			// object-contain at a small size is how logos are drawn; a small photo with a large
			// version (a thumbnail) is still a photo.
			if strings.Contains(class, "object-contain") || widest < r.MinSide {
				tiny = true
			}
		}
	}
	if widest >= 1200 {
		bonus += 10
	}
	judge(src, attr(n, "alt"), section, bonus, tiny)
}

// rank orders picks best first and keeps one of each picture.
func rank(picks []Pick) []Pick {
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].score != picks[j].score {
			return picks[i].score > picks[j].score
		}
		return picks[i].order < picks[j].order
	})
	seen := map[string]bool{}
	out := picks[:0]
	for _, p := range picks {
		if seen[p.key] {
			continue
		}
		seen[p.key] = true
		out = append(out, p)
	}
	return out
}

// InnerPages returns the site's own pages worth reading for pictures (services, about, products,
// projects, blog...), in the order the rules list them, at most r.MaxPages.
func InnerPages(page, pageURL string, r SiteRules) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil
	}
	host := strings.TrimPrefix(strings.ToLower(base.Hostname()), "www.")
	found := map[string]string{} // first path part -> address
	for _, h := range scanPage(doc).links {
		u := normaliseHTTPS(h, base)
		if u == "" {
			continue
		}
		pu, err := url.Parse(u)
		if err != nil || strings.TrimPrefix(strings.ToLower(pu.Hostname()), "www.") != host {
			continue
		}
		first := strings.ToLower(strings.SplitN(strings.Trim(pu.Path, "/"), "/", 2)[0])
		if first == "" || strings.Contains(strings.Trim(pu.Path, "/"), "/") {
			continue // the home page itself, or a deeper page: the section's index is enough
		}
		if _, ok := found[first]; !ok {
			pu.RawQuery, pu.Fragment = "", ""
			found[first] = pu.String()
		}
	}
	var out []string
	for _, p := range r.Pages {
		if u, ok := found[p]; ok {
			out = append(out, u)
			delete(found, p)
			if len(out) >= r.MaxPages {
				break
			}
		}
	}
	return out
}

// FromLabel says where a picture came from for the portal: "Services page" or "Home page, hero".
func FromLabel(page, section string) string {
	if page == "" {
		return ""
	}
	name := "Home"
	if u, err := url.Parse(page); err == nil {
		if p := strings.Trim(u.Path, "/"); p != "" {
			name = strings.ReplaceAll(path.Base(p), "-", " ")
			name = strings.ToUpper(name[:1]) + name[1:]
		}
	}
	label := name + " page"
	if section != "" && !strings.EqualFold(section, name) && !strings.EqualFold(section+"s", name) {
		label += ", " + section
	}
	return label
}
