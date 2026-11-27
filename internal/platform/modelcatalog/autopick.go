package modelcatalog

import (
	"regexp"
	"strings"
)

// Automatic upgrades (owner, 2026-11-28): when the owner presses Sync models on the fal.ai or
// ElevenLabs card, a media route moves to a newer model of its own family and tier on its own
// (eleven_v4 to eleven_v5, elevenlabs/tts/eleven-v4-turbo to elevenlabs/tts/eleven-v5-turbo,
// fal-ai/kling-video/v3/pro/image-to-video to .../v3.5/pro/...). A different family, a different
// variant (standard to pro, lite to full) or a preview, alpha or beta build is never switched to:
// those stay recommendations for the owner to compare.

// versionRe is a version number inside an id: digits with optional dot or underscore parts
// ("2.3", "v4", "5_5"). Sizes and snapshot dates are not versions and are handled by version().
var versionRe = regexp.MustCompile(`\d+(?:[._]\d+)*`)

// prerelease marks ids that are not a released build.
var prerelease = regexp.MustCompile(`(?i)(preview|alpha|beta|\bexp\b|experimental|\brc\d*\b|nightly|canary|\bdev\b|test)`)

// Family is an id with every version number replaced by "#": two ids of one family and variant
// differ only in their numbers.
func Family(id string) string {
	return versionRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(id)), "#")
}

// NewerInFamily returns the newest model in models of current's family that is a higher version
// than current and a released build, or "" when current is already the newest. A model whose
// category differs from current's (when both are known) is not the same tier and is skipped.
func NewerInFamily(current string, models []Model) string {
	current = strings.TrimSpace(current)
	if current == "" || versionRe.FindString(current) == "" {
		return "" // an id without a version (creatify/boreal) has no newer version to find
	}
	fam := Family(current)
	cat := ""
	for _, m := range models {
		if m.ID == current {
			cat = m.Category
		}
	}
	best := ""
	for _, m := range models {
		if m.ID == current || Family(m.ID) != fam || prerelease.MatchString(m.ID) {
			continue
		}
		if cat != "" && m.Category != "" && m.Category != cat {
			continue
		}
		if !versionAbove(m.ID, current) {
			continue
		}
		if best == "" || versionAbove(m.ID, best) {
			best = m.ID
		}
	}
	return best
}

// versionAbove reports whether a's version numbers are higher than b's, read in order.
func versionAbove(a, b string) bool {
	va, vb := versionNums(a), versionNums(b)
	for i := 0; i < len(va) || i < len(vb); i++ {
		x, y := 0, 0
		if i < len(va) {
			x = va[i]
		}
		if i < len(vb) {
			y = vb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// versionNums is every number in an id, in order ("hailuo-2.3-fast" is 2, 3).
func versionNums(id string) []int {
	var out []int
	for _, s := range numRe.FindAllString(id, -1) {
		n := 0
		for _, c := range s {
			n = n*10 + int(c-'0')
			if n > 1<<30 {
				break
			}
		}
		out = append(out, n)
	}
	return out
}
