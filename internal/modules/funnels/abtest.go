package funnels

import (
	"errors"
	"strings"
)

// Sprint 7 #53: funnel A/B testing. Funnel.metadata["ab_test"] holds a second version (B) of the
// page's headline, subheadline and hero image and the share of visitors who see it. A visitor
// keeps one bucket (0-99, a cookie set by the page); a bucket below the split sees B. Each lead
// records the version it saw (lead metadata "funnel_variant"), and the analytics compare them.

// MetaABTest is the Funnel.metadata key of the test.
const MetaABTest = "ab_test"

const (
	minABSplit   = 10
	maxABSplit   = 90
	maxABTextLen = 200
)

// ABVariant is version B's copy; an empty field shows version A's.
type ABVariant struct {
	Headline     string `json:"headline,omitempty"`
	Subheadline  string `json:"subheadline,omitempty"`
	HeroImageURL string `json:"hero_image_url,omitempty"`
}

// ABTest is a funnel's A/B test.
type ABTest struct {
	Enabled bool      `json:"enabled"`
	Split   int       `json:"split"` // percent of visitors who see B
	B       ABVariant `json:"b"`
}

// ReadABTest reads the test from funnel metadata.
func ReadABTest(meta map[string]any) ABTest {
	m, _ := meta[MetaABTest].(map[string]any)
	if m == nil {
		return ABTest{}
	}
	t := ABTest{}
	t.Enabled, _ = m["enabled"].(bool)
	if n, ok := m["split"].(float64); ok {
		t.Split = int(n)
	}
	if b, ok := m["b"].(map[string]any); ok {
		str := func(k string) string { s, _ := b[k].(string); return strings.TrimSpace(s) }
		t.B = ABVariant{Headline: str("headline"), Subheadline: str("subheadline"), HeroImageURL: str("hero_image_url")}
	}
	return t
}

// ValidateABTest checks the ab_test key of a funnel metadata update (null or absent: no test).
func ValidateABTest(meta map[string]any) error {
	raw, ok := meta[MetaABTest]
	if !ok || raw == nil {
		return nil
	}
	if _, isMap := raw.(map[string]any); !isMap {
		return errors.New("ab_test must be an object")
	}
	t := ReadABTest(meta)
	if !t.Enabled {
		return nil
	}
	if t.Split < minABSplit || t.Split > maxABSplit {
		return errors.New("ab_test.split must be from 10 to 90 percent")
	}
	if t.B.Headline == "" && t.B.Subheadline == "" && t.B.HeroImageURL == "" {
		return errors.New("version B needs a different headline, subheadline or hero image")
	}
	if len(t.B.Headline) > maxABTextLen || len(t.B.Subheadline) > maxABTextLen {
		return errors.New("version B's headline and subheadline are limited to 200 characters")
	}
	if t.B.HeroImageURL != "" && (!strings.HasPrefix(t.B.HeroImageURL, "https://") || len(t.B.HeroImageURL) > 1000) {
		return errors.New("version B's hero image must be an https link")
	}
	return nil
}

// VariantFor is the version a visitor bucket (0-99) sees.
func VariantFor(t ABTest, bucket int) string {
	if t.Enabled && bucket >= 0 && bucket < t.Split {
		return "b"
	}
	return "a"
}

// NormalVariant is the version a lead submission names, or "" when it names none.
func NormalVariant(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "a":
		return "a"
	case "b":
		return "b"
	}
	return ""
}

// PublicABTest is the running test as the public page needs it.
type PublicABTest struct {
	Split int       `json:"split"`
	B     ABVariant `json:"b"`
}

func publicABTest(meta map[string]any) *PublicABTest {
	t := ReadABTest(meta)
	if !t.Enabled || t.Split < minABSplit || t.Split > maxABSplit {
		return nil
	}
	// Rows saved before validation are cleaned here, not trusted.
	if !strings.HasPrefix(t.B.HeroImageURL, "https://") || len(t.B.HeroImageURL) > 1000 {
		t.B.HeroImageURL = ""
	}
	if len(t.B.Headline) > maxABTextLen || len(t.B.Subheadline) > maxABTextLen {
		return nil
	}
	if t.B.Headline == "" && t.B.Subheadline == "" && t.B.HeroImageURL == "" {
		return nil
	}
	return &PublicABTest{Split: t.Split, B: t.B}
}
