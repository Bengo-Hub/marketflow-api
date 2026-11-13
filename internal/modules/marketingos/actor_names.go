package marketingos

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ActorNameStyles are the naming traditions a business may choose for the people in its videos
// (owner, 2026-11-12): local Kenyan communities, the Swahili coast, neutral names, or a mix.
var ActorNameStyles = []string{"kikuyu", "luo", "kalenjin", "kamba", "maasai", "swahili_coast", "neutral", "mixed"}

// Limits of the exact names list.
const (
	MaxActorNames   = 20
	MaxActorNameLen = 40
)

// validateActorNaming checks the style and cleans the names: trimmed, blanks and repeats (any
// case) dropped. A name holds letters, spaces, apostrophes, hyphens and dots only, since it goes
// into the script writer's prompt word for word.
func (b *BrandContext) validateActorNaming() error {
	b.ActorNameStyle = strings.ToLower(strings.TrimSpace(b.ActorNameStyle))
	if b.ActorNameStyle != "" && !validStyle(b.ActorNameStyle) {
		return fmt.Errorf("actor_name_style must be one of %s", strings.Join(ActorNameStyles, ", "))
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(b.ActorNames))
	for _, n := range b.ActorNames {
		// A line break or other control character could start a new instruction in the
		// writer's prompt, so it is refused before spaces are tidied.
		if strings.ContainsFunc(n, unicode.IsControl) {
			return fmt.Errorf("actor name %q may use letters, spaces, apostrophes, hyphens and dots only", strings.TrimSpace(n))
		}
		n = strings.Join(strings.Fields(n), " ")
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		if utf8.RuneCountInString(n) > MaxActorNameLen {
			return fmt.Errorf("actor names are limited to %d characters each", MaxActorNameLen)
		}
		if !validName(n) {
			return fmt.Errorf("actor name %q may use letters, spaces, apostrophes, hyphens and dots only", n)
		}
		seen[strings.ToLower(n)] = true
		clean = append(clean, n)
	}
	if len(clean) > MaxActorNames {
		return fmt.Errorf("at most %d actor names", MaxActorNames)
	}
	if len(clean) == 0 {
		clean = nil
	}
	b.ActorNames = clean
	return nil
}

func validStyle(s string) bool {
	for _, x := range ActorNameStyles {
		if x == s {
			return true
		}
	}
	return false
}

func validName(n string) bool {
	for _, r := range n {
		switch {
		case unicode.IsLetter(r), unicode.Is(unicode.Mn, r), r == ' ', r == '\'', r == '’', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// ActorNaming reads the naming choice from a tenant's metadata, for callers that hold the tenant
// row (the creative worker's script call).
func ActorNaming(meta map[string]any) (style string, names []string) {
	raw, ok := meta[MetadataKey].(map[string]any)
	if !ok {
		return "", nil
	}
	style, _ = raw["actor_name_style"].(string)
	if list, ok := raw["actor_names"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				names = append(names, s)
			}
		}
	}
	return style, names
}
