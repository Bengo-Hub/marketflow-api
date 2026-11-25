// Package postcaption writes a post's caption (owner, 2026-11-25): the ad's words, the end card's
// real contacts, the ad's lead page link and, for a pharmacy's medicine ad, its PPB approval
// line. The Studio's post dialog, Approve and post on WhatsApp and the ad autopilot all use it, so
// a video gets the same caption wherever it is approved.
package postcaption

import (
	"net/url"
	"strings"
	"unicode/utf16"
)

// Limit is the longest caption every platform takes: Instagram's 2,200 characters and TikTok's
// 2,200 UTF-16 units (publishing preflight); YouTube's description and Facebook take more.
const Limit = 2200

// Parts are what a caption is made of; empty parts are left out.
type Parts struct {
	// Words are the ad's own words. Their first line stays first: YouTube takes it as the title.
	Words string
	// Title stands in as the first line when the ad has no words.
	Title    string
	Contacts []string
	Link     string
	// PPB is the pharmacy approval line; it comes last and is never cut.
	PPB string
}

// Compose writes the caption within Limit. When it is too long the words are cut at a word, never
// the contacts, the link or the approval line.
func Compose(p Parts) string {
	words := strings.TrimSpace(p.Words)
	if words == "" {
		words = strings.TrimSpace(p.Title)
	}
	var tail []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || strings.Contains(words, s) {
			return
		}
		for _, t := range tail {
			if strings.Contains(t, s) {
				return
			}
		}
		tail = append(tail, s)
	}
	var contacts []string
	for _, c := range p.Contacts {
		if c = strings.TrimSpace(c); c != "" && !strings.Contains(words, c) {
			contacts = append(contacts, c)
		}
	}
	add(strings.Join(contacts, "\n"))
	add(p.Link)
	add(p.PPB)
	end := strings.Join(tail, "\n\n")
	if end == "" {
		return cut(words, Limit)
	}
	if words == "" {
		return cut(end, Limit)
	}
	end = "\n\n" + end
	return cut(words, Limit-units(end)) + end
}

func units(s string) int { return len(utf16.Encode([]rune(s))) }

// cut shortens s to max UTF-16 units at a word.
func cut(s string, max int) string {
	if units(s) <= max {
		return s
	}
	n, end := 0, 0
	for i, r := range s {
		u := utf16.RuneLen(r)
		if u < 0 {
			u = 1
		}
		if n+u > max {
			end = i
			break
		}
		n += u
	}
	out := s[:end]
	if i := strings.LastIndexAny(out, " \n"); i > len(out)/2 {
		out = out[:i]
	}
	return strings.TrimSpace(out)
}

// Words are the ad's own words from its copy: its caption or text, else the headline, subline and
// call to action (on the copy of a poster, on the end card of a video ad), one per line.
func Words(adCopy map[string]any) string {
	for _, k := range []string{"caption", "primary_text", "body"} {
		if s, ok := adCopy[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	card, _ := adCopy["end_card"].(map[string]any)
	var lines []string
	for _, k := range []string{"headline", "subline", "cta"} {
		s, _ := adCopy[k].(string)
		if strings.TrimSpace(s) == "" {
			s, _ = card[k].(string)
		}
		if strings.TrimSpace(s) != "" {
			lines = append(lines, strings.TrimSpace(s))
		}
	}
	return strings.Join(lines, "\n")
}

// PPBLine is the pharmacy approval line of a version's metadata (metadata.ppb_approval), or "".
func PPBLine(meta map[string]any) string {
	ppb, _ := meta["ppb_approval"].(map[string]any)
	s, _ := ppb["caption_line"].(string)
	return strings.TrimSpace(s)
}

// LeadURL is an ad's lead page with UTM tags, so each lead shows which ad and channel brought it.
// marketflow-ui builds the same link (lib/urls adLeadUrl). "" without a base or a slug.
func LeadURL(base, slug, briefID, source, medium string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || slug == "" {
		return ""
	}
	id := briefID
	if len(id) > 8 {
		id = id[:8]
	}
	q := url.Values{"utm_source": {source}, "utm_medium": {medium}, "utm_campaign": {"ad-" + id}}
	return base + "/f/" + url.PathEscape(slug) + "?" + q.Encode()
}
