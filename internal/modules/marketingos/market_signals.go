package marketingos

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Find, Flip, Fix inputs (docs/integrations/find-flip-fix-engine.md). They live in Tenant.metadata
// next to the competitor list, are edited on the same settings, and reach marketflow-ai as one
// capped MarketSignals block in the OS context.
const (
	ReferenceAdsKey       = "reference_ads"
	CustomerComplaintsKey = "customer_complaints"
	CompetitorScanKey     = "competitor_scan"

	MaxReferenceAds     = 20
	MaxReferenceNotes   = 300
	MaxReferenceURL     = 500
	MaxComplaints       = 20
	MaxComplaintText    = 200
	maxSignalPatterns   = 10
	maxSignalReferences = 10
)

// ErrInvalidSettings is a settings value the owner must correct (answered as 400).
var ErrInvalidSettings = errors.New("invalid settings")

// ReferenceAd is an ad the owner saw working (Meta Ad Library, TikTok Creative Center, anywhere),
// pasted as a link with the owner's own notes on what makes it work. Nothing is scraped.
type ReferenceAd struct {
	URL      string `json:"url"`
	Platform string `json:"platform"`
	Notes    string `json:"notes"`
}

var referencePlatforms = map[string]bool{"meta": true, "tiktok": true, "youtube": true, "google": true, "other": true}

// cleanReferenceAds trims, validates and dedupes by link. A bad entry is an error, so the owner
// sees which one to fix instead of losing it quietly.
func cleanReferenceAds(in []ReferenceAd) ([]ReferenceAd, error) {
	out := make([]ReferenceAd, 0, len(in))
	seen := map[string]bool{}
	for i, r := range in {
		r.URL, r.Notes = strings.TrimSpace(r.URL), strings.TrimSpace(r.Notes)
		r.Platform = strings.ToLower(strings.TrimSpace(r.Platform))
		if r.URL == "" && r.Notes == "" {
			continue
		}
		u, err := url.Parse(r.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(r.URL) > MaxReferenceURL {
			return nil, fmt.Errorf("reference ad %d: use a full web link (https://...) of up to %d characters", i+1, MaxReferenceURL)
		}
		if r.Platform == "" {
			r.Platform = "other"
		}
		if !referencePlatforms[r.Platform] {
			return nil, fmt.Errorf("reference ad %d: platform must be meta, tiktok, youtube, google or other", i+1)
		}
		if utf8.RuneCountInString(r.Notes) > MaxReferenceNotes {
			return nil, fmt.Errorf("reference ad %d: notes are limited to %d characters", i+1, MaxReferenceNotes)
		}
		key := strings.ToLower(r.URL)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	if len(out) > MaxReferenceAds {
		return nil, fmt.Errorf("keep at most %d reference ads", MaxReferenceAds)
	}
	return out, nil
}

// cleanComplaints trims, bounds and dedupes the complaints the owner hears about the market.
func cleanComplaints(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" || seen[strings.ToLower(c)] {
			continue
		}
		if utf8.RuneCountInString(c) > MaxComplaintText {
			return nil, fmt.Errorf("complaints are limited to %d characters each", MaxComplaintText)
		}
		seen[strings.ToLower(c)] = true
		out = append(out, c)
	}
	if len(out) > MaxComplaints {
		return nil, fmt.Errorf("keep at most %d complaints", MaxComplaints)
	}
	return out, nil
}

func readReferenceAds(meta map[string]any) []ReferenceAd {
	var out []ReferenceAd
	if raw, ok := meta[ReferenceAdsKey]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func readStrings(meta map[string]any, key string) []string {
	var out []string
	if list, ok := meta[key].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// MarketSignals is what the writers learn from the market. Patterns describe winning competitor
// ads by shape only (format, how long they ran), never their text or the advertiser's name, so
// nothing can be copied and no rival is named. References and complaints are the owner's own
// words.
//
// A winner the scan tagged adds its decide tags (hook, angle, offer, relevance) to its pattern;
// tags are fixed labels, never the ad's words. Summary is the scan's brief for the Ads Specialist,
// written by marketflow-ai from those tags and shapes only.
type MarketSignals struct {
	Patterns   []string      `json:"patterns,omitempty"`
	Summary    string        `json:"summary,omitempty"`
	References []ReferenceAd `json:"references,omitempty"`
	Complaints []string      `json:"complaints,omitempty"`
	// Competitors are the names the owner configured. Only the "What has worked before"
	// suggestion reads them, to recall what is known to work for those businesses; the ad
	// writers' market block never does, so no ad names a rival.
	Competitors []string `json:"competitors,omitempty"`
}

// maxSignalCompetitors caps the competitor names sent to the AI.
const maxSignalCompetitors = 10

// Empty reports whether there is nothing to learn from yet.
func (m MarketSignals) Empty() bool {
	return len(m.Patterns) == 0 && len(m.References) == 0 && len(m.Complaints) == 0 && len(m.Competitors) == 0
}

// marketSignals builds the capped block from the tenant's metadata.
func marketSignals(meta map[string]any) MarketSignals {
	var out MarketSignals
	if scan, ok := meta[CompetitorScanKey].(map[string]any); ok {
		if s, _ := scan["summary"].(string); strings.TrimSpace(s) != "" {
			out.Summary = clipRunes(strings.TrimSpace(s), maxScanSummary)
		}
		list, _ := scan["competitors"].([]any)
	competitors:
		for _, e := range list {
			c, _ := e.(map[string]any)
			winners, _ := c["winners"].([]any)
			for _, w := range winners {
				if len(out.Patterns) >= maxSignalPatterns {
					break competitors
				}
				if p := winnerPattern(w); p != "" {
					out.Patterns = append(out.Patterns, p)
				}
			}
		}
	}
	refs := readReferenceAds(meta)
	if len(refs) > maxSignalReferences {
		refs = refs[:maxSignalReferences]
	}
	out.References = refs
	out.Complaints = readStrings(meta, CustomerComplaintsKey)
	out.Competitors = readStrings(meta, CompetitorsKey)
	if len(out.Competitors) > maxSignalCompetitors {
		out.Competitors = out.Competitors[:maxSignalCompetitors]
	}
	if len(out.Patterns) == 0 {
		out.Summary = "" // a summary only explains winners the writer can see
	}
	return out
}

// maxScanSummary caps the scan's summary in the writer's prompt.
const maxScanSummary = 400

// winnerPattern describes one winner by shape and its tags, for example "video ad that ran 92
// days, still running; hook: question; angle: pain; offer: discount; relevance: related".
func winnerPattern(w any) string {
	m, _ := w.(map[string]any)
	if m == nil {
		return ""
	}
	format, _ := m["format"].(string)
	days, _ := m["total_days_shown"].(float64)
	if format == "" || days < 1 {
		return ""
	}
	p := fmt.Sprintf("%s ad that ran %d days", strings.ToLower(format), int(days))
	if running, _ := m["still_running"].(bool); running {
		p += ", still running"
	}
	if t := readWinnerTags(m["tags"]); t != nil {
		for _, f := range []struct{ label, v string }{{"hook", t.HookType}, {"angle", t.Angle}, {"offer", t.OfferType}, {"relevance", t.Relevance}} {
			if f.v != "" {
				p += "; " + f.label + ": " + strings.ReplaceAll(f.v, "_", " ")
			}
		}
	}
	return p
}

// WinnerTags are the decide labels marketflow-ai put on a top winner. They come from fixed lists
// (marketflow-ai adintel/tags.go), so anything that is not a short lowercase label is dropped.
type WinnerTags struct {
	HookType  string `json:"hook_type,omitempty"`
	Angle     string `json:"angle,omitempty"`
	OfferType string `json:"offer_type,omitempty"`
	Relevance string `json:"relevance,omitempty"`
}

func validTag(s string) string {
	if s == "" || len(s) > 30 {
		return ""
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '_' && r != ' ' {
			return ""
		}
	}
	return s
}

// readWinnerTags reads a stored winner's tags; nil when it has none.
func readWinnerTags(raw any) *WinnerTags {
	m, _ := raw.(map[string]any)
	if m == nil {
		return nil
	}
	t := WinnerTags{HookType: validTag(str(m["hook_type"])), Angle: validTag(str(m["angle"])),
		OfferType: validTag(str(m["offer_type"])), Relevance: validTag(str(m["relevance"]))}
	if t == (WinnerTags{}) {
		return nil
	}
	return &t
}

// maxScanBrand caps the brand line the scan's relevance question compares against.
const maxScanBrand = 600

// ScanBrand is the short line of the tenant's own brand context the weekly scan sends to
// marketflow-ai, so the winners can be scored for relevance: the positioning, audience and offers.
// Empty when there is no brand context yet.
func ScanBrand(meta map[string]any) string {
	raw, _ := meta[MetadataKey].(map[string]any)
	if raw == nil {
		return ""
	}
	parts := []string{}
	if p := strings.TrimSpace(str(raw["positioning"])); p != "" {
		parts = append(parts, p)
	}
	if a := readList(raw["audience"]); len(a) > 0 {
		parts = append(parts, "Audience: "+strings.Join(a, ", "))
	}
	if o := brandOffers(meta); len(o) > 0 {
		parts = append(parts, "Offers: "+strings.Join(o, ", "))
	}
	return clipRunes(strings.Join(parts, ". "), maxScanBrand)
}

func readList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s := strings.TrimSpace(str(e)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// clipRunes cuts s to n runes.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
