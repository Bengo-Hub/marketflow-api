package publishing

import (
	"strings"
	"testing"
)

func TestPreflight(t *testing.T) {
	ok := Video{DurationMS: 30_000, Width: 1080, Height: 1920, FPS: 30, Bytes: 20 << 20}
	fbScopes := []string{"pages_show_list", "pages_read_engagement", "pages_manage_posts"}

	if got := Preflight(Facebook, ok, Post{Caption: "Holiday coding for kids"}, fbScopes, 0); len(got) != 0 {
		t.Fatalf("a valid reel was refused: %v", got)
	}

	cases := []struct {
		name, platform string
		v              Video
		p              Post
		scopes         []string
		maxSec         int
		want           string
	}{
		{"too short", Facebook, Video{DurationMS: 2000, Width: 1080, Height: 1920}, Post{}, fbScopes, 0, "at least 3 s"},
		{"reel over 90 s", Facebook, Video{DurationMS: 95_000, Width: 1080, Height: 1920}, Post{}, fbScopes, 0, "at most 90 s"},
		{"landscape short", YouTube, Video{DurationMS: 30_000, Width: 1920, Height: 1080}, Post{}, []string{"https://www.googleapis.com/auth/youtube.upload"}, 0, "vertical"},
		{"missing scope", Facebook, ok, Post{}, []string{"pages_show_list"}, 0, `"pages_manage_posts"`},
		{"instagram caption", Instagram, ok, Post{Caption: strings.Repeat("a", 2201)}, []string{"instagram_basic", "instagram_content_publish", "pages_read_engagement"}, 0, "2201 characters"},
		{"instagram hashtags", Instagram, ok, Post{Caption: strings.Repeat("#tag ", 31)}, []string{"instagram_basic", "instagram_content_publish", "pages_read_engagement"}, 0, "30 hashtags"},
		{"tiktok creator cap", TikTok, Video{DurationMS: 200_000, FPS: 30}, Post{}, []string{"video.publish"}, 180, "at most 180 s"},
		{"fps out of range", TikTok, Video{DurationMS: 10_000, FPS: 15}, Post{}, []string{"video.publish"}, 0, "23 to 60 fps"},
		{"unknown platform", "myspace", ok, Post{}, nil, 0, "not a publishing platform"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(Preflight(c.platform, c.v, c.p, c.scopes, c.maxSec), " | ")
			if !strings.Contains(got, c.want) {
				t.Fatalf("want %q in %q", c.want, got)
			}
		})
	}

	// TikTok counts the caption in UTF-16 units: an emoji is two.
	emoji := strings.Repeat("😀", 1101) // 2,202 UTF-16 units, 1,101 runes
	if got := Preflight(TikTok, Video{DurationMS: 10_000, FPS: 30}, Post{Caption: emoji}, []string{"video.publish"}, 0); len(got) == 0 {
		t.Fatal("a TikTok title over 2,200 UTF-16 units must be refused")
	}
}
