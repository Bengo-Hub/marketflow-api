package funnels

import (
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

// landingPolicy allows the layout and text markup a hand-written landing page needs
// (sections, headings, images, tables, links, classes and a short list of inline
// styles) and drops scripts, event handlers, iframes and forms. The lead form and the
// chat widget are added by the server when it renders the page, never by the HTML.
var landingPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowElements("section", "header", "footer", "main", "article", "aside", "nav", "figure", "figcaption", "span", "div")
	p.AllowAttrs("class", "id").Globally()
	p.AllowStyles(
		"color", "background-color", "text-align", "font-size", "font-weight", "font-style",
		"line-height", "margin", "margin-top", "margin-bottom", "padding", "border",
		"border-radius", "width", "max-width", "height", "display", "gap",
	).Globally()
	p.AllowURLSchemes("https", "http", "mailto", "tel")
	p.RequireNoFollowOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}()

// SanitizeLandingHTML returns landing page HTML that is safe to serve on a public page.
func SanitizeLandingHTML(s string) string {
	return strings.TrimSpace(landingPolicy.Sanitize(s))
}

func sanitizeLandingPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := SanitizeLandingHTML(*p)
	return &v
}

var hexColor = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// SafeColor returns c when it is a #rgb or #rrggbb colour, otherwise fallback. Funnel
// colours are written into a <style> block, so anything else could break out of it.
func SafeColor(c, fallback string) string {
	if hexColor.MatchString(strings.TrimSpace(c)) {
		return strings.TrimSpace(c)
	}
	return fallback
}
