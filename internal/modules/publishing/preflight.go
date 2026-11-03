package publishing

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// spec is a platform's documented limits for a short vertical video (official docs, checked
// 2026-11-03; see docs/integrations/organic-publishing-apis.md). Zero means no documented limit.
type spec struct {
	MinSec, MaxSec   float64
	MinFPS, MaxFPS   float64
	MaxBytes         int64
	MaxCaption       int // characters (TikTok counts UTF-16 units)
	MaxHashtags      int
	MaxMentions      int
	RequiredScopes   []string
	captionUTF16     bool
	verticalRequired bool
}

var specs = map[string]spec{
	// Facebook Page Reels: 3 to 90 s, 24 to 60 fps, 9:16.
	Facebook: {MinSec: 3, MaxSec: 90, MinFPS: 24, MaxFPS: 60, verticalRequired: true,
		RequiredScopes: []string{"pages_show_list", "pages_read_engagement", "pages_manage_posts"}},
	// Instagram Reels: 3 s to 15 min, 23 to 60 fps, 300 MB, caption 2,200 chars, 30 hashtags, 20 @ tags.
	Instagram: {MinSec: 3, MaxSec: 900, MinFPS: 23, MaxFPS: 60, MaxBytes: 300 << 20, MaxCaption: 2200, MaxHashtags: 30, MaxMentions: 20,
		RequiredScopes: []string{"instagram_basic", "instagram_content_publish", "pages_read_engagement"}},
	// YouTube: up to 256 GB; a Short is vertical.
	YouTube: {MaxBytes: 256 << 30, verticalRequired: true,
		RequiredScopes: []string{"https://www.googleapis.com/auth/youtube.upload"}},
	// TikTok: 23 to 60 fps, 4 GB, title 2,200 UTF-16 units; the duration cap comes from creator_info.
	TikTok: {MinFPS: 23, MaxFPS: 60, MaxBytes: 4 << 30, MaxCaption: 2200, captionUTF16: true,
		RequiredScopes: []string{"video.publish"}},
}

// Preflight returns every reason this video and caption cannot be posted to the platform
// (gate G6), in plain words for the owner. An empty slice means it may go ahead. maxSecOverride
// is TikTok's creator_info max_video_post_duration_sec, 0 when not known.
func Preflight(platform string, v Video, p Post, scopes []string, maxSecOverride int) []string {
	sp, ok := specs[platform]
	if !ok {
		return []string{fmt.Sprintf("%s is not a publishing platform", platform)}
	}
	var out []string
	sec := float64(v.DurationMS) / 1000
	maxSec := sp.MaxSec
	if maxSecOverride > 0 {
		maxSec = float64(maxSecOverride)
	}
	if sp.MinSec > 0 && sec < sp.MinSec {
		out = append(out, fmt.Sprintf("the video is %.0f s; %s needs at least %.0f s", sec, title(platform), sp.MinSec))
	}
	if maxSec > 0 && sec > maxSec {
		out = append(out, fmt.Sprintf("the video is %.0f s; %s allows at most %.0f s", sec, title(platform), maxSec))
	}
	if v.FPS > 0 && sp.MinFPS > 0 && (v.FPS < sp.MinFPS || v.FPS > sp.MaxFPS) {
		out = append(out, fmt.Sprintf("the video runs at %.0f fps; %s needs %.0f to %.0f fps", v.FPS, title(platform), sp.MinFPS, sp.MaxFPS))
	}
	if sp.MaxBytes > 0 && v.Bytes > sp.MaxBytes {
		out = append(out, fmt.Sprintf("the file is %d MB; %s allows at most %d MB", v.Bytes>>20, title(platform), sp.MaxBytes>>20))
	}
	if sp.verticalRequired && v.Width > 0 && v.Height > 0 && v.Width >= v.Height {
		out = append(out, fmt.Sprintf("%s needs a vertical video (9:16)", title(platform)))
	}
	if sp.MaxCaption > 0 {
		n := utf8.RuneCountInString(p.Caption)
		if sp.captionUTF16 {
			n = len(utf16.Encode([]rune(p.Caption)))
		}
		if n > sp.MaxCaption {
			out = append(out, fmt.Sprintf("the caption is %d characters; %s allows %d", n, title(platform), sp.MaxCaption))
		}
	}
	if sp.MaxHashtags > 0 && countPrefixed(p.Caption, '#') > sp.MaxHashtags {
		out = append(out, fmt.Sprintf("%s allows at most %d hashtags", title(platform), sp.MaxHashtags))
	}
	if sp.MaxMentions > 0 && countPrefixed(p.Caption, '@') > sp.MaxMentions {
		out = append(out, fmt.Sprintf("%s allows at most %d @ mentions", title(platform), sp.MaxMentions))
	}
	for _, s := range sp.RequiredScopes {
		if !slices.Contains(scopes, s) {
			out = append(out, fmt.Sprintf("the %s connection is missing the %q permission; reconnect it", title(platform), s))
		}
	}
	return out
}

// MissingScopes lists the permissions the platform needs that the connection lacks.
func MissingScopes(platform string, scopes []string) []string {
	var out []string
	for _, s := range specs[platform].RequiredScopes {
		if !slices.Contains(scopes, s) {
			out = append(out, s)
		}
	}
	return out
}

// countPrefixed counts words that start with mark (# or @).
func countPrefixed(s string, mark byte) int {
	n := 0
	for _, w := range strings.Fields(s) {
		if len(w) > 1 && w[0] == mark {
			n++
		}
	}
	return n
}

// Title is the platform's display name (Facebook, Instagram, YouTube, TikTok).
func Title(platform string) string { return title(platform) }

func title(platform string) string {
	switch platform {
	case YouTube:
		return "YouTube"
	case TikTok:
		return "TikTok"
	default:
		return strings.ToUpper(platform[:1]) + platform[1:]
	}
}
