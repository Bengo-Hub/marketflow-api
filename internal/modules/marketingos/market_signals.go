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
type MarketSignals struct {
	Patterns   []string      `json:"patterns,omitempty"`
	References []ReferenceAd `json:"references,omitempty"`
	Complaints []string      `json:"complaints,omitempty"`
}

// Empty reports whether there is nothing to learn from yet.
func (m MarketSignals) Empty() bool {
	return len(m.Patterns) == 0 && len(m.References) == 0 && len(m.Complaints) == 0
}

// marketSignals builds the capped block from the tenant's metadata.
func marketSignals(meta map[string]any) MarketSignals {
	var out MarketSignals
	if scan, ok := meta[CompetitorScanKey].(map[string]any); ok {
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
	return out
}

// winnerPattern describes one winner by shape, for example "video ad that ran 92 days, still
// running".
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
	return p
}
