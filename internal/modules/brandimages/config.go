// Package brandimages keeps a tenant's brand images: 3 to 5 pictures of the business (the shop,
// the team, the products in use) that posters and video ads may use next to an offering's own
// photos. The files are private objects under the tenant's key prefix
// (marketflow/<tenant_id>/brand-images/...), reachable only through short-lived signed links. The
// list lives in Tenant.metadata.brand_images; there is no table of its own.
package brandimages

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware/objectstore"
)

// MediaType is the object store media type of a brand image.
const MediaType = "brand-images"

// MetadataKey is where the list lives in Tenant.metadata. Only this package writes it.
const MetadataKey = "brand_images"

// Config holds the limits. DefaultConfig is the one place the defaults are set; ConfigFromEnv
// overrides them.
type Config struct {
	// Min is the number of images that completes the onboarding step.
	Min int
	// Max is the most a tenant may keep.
	Max int
	// MaxBytes caps one upload.
	MaxBytes int64
	// MinShortSide is the smallest accepted short side in pixels.
	MinShortSide int
	// MaxScreens and MinScreenSide are the same limits for screenshots (kinds.go).
	MaxScreens    int
	MinScreenSide int
	// ListLinkTTL is the lifetime of the links the portal shows.
	ListLinkTTL time.Duration
	// RenderLinkTTL is the lifetime of the links sent to marketflow-ai with a render, long enough
	// for a queued video job to fetch them.
	RenderLinkTTL time.Duration
	// SiteSweepEvery is how often the sweep links website pictures for tenants below Min.
	SiteSweepEvery time.Duration
	// SiteRetryAfter is how long the sweep waits before reading the same tenant's site again.
	SiteRetryAfter time.Duration
	// SiteSweepLimit is the most tenants one sweep reads.
	SiteSweepLimit int
	// Site says which website pictures are taken (pick.go).
	Site SiteRules
}

// SiteRules decide which pictures of a website are the business's own. The word lists are matched
// against the words of a picture's surroundings (ids, classes, labels and headings of the parts of
// the page it sits in), so "Trusted by businesses across Kenya" or class "clientLogos" both count.
type SiteRules struct {
	// Pages are the inner pages read besides the home page, by the first part of their path.
	Pages []string
	// MaxPages is the most inner pages read per site.
	MaxPages int
	// ExcludeWords mark parts of a page whose pictures belong to someone else or are page chrome:
	// client and partner strips, logo marquees, testimonials.
	ExcludeWords []string
	// PreferWords mark the parts that show the business itself: hero, services, about, products,
	// projects, blog, call to action.
	PreferWords []string
	// LogoWords in a picture's file name, path or description mark a logo or an icon.
	LogoWords []string
	// MinSide is the smallest display size (px) a picture may be shown at to count as a photo.
	MinSide int
	// ReadWindow bounds reading the site and its pictures, so the portal gets its answer in time.
	ReadWindow time.Duration
}

// DefaultSiteRules is the starting point; every list can be replaced from the environment.
func DefaultSiteRules() SiteRules {
	return SiteRules{
		Pages: []string{"services", "service", "about", "about-us", "products", "product", "shop",
			"projects", "project", "portfolio", "work", "our-work", "gallery", "blog", "news", "menu", "team"},
		MaxPages: 5,
		ExcludeWords: []string{"client", "customer", "partner", "sponsor", "trusted", "marquee", "ticker",
			"logo", "brands", "testimonial", "review", "avatar", "press", "award", "certification",
			"accreditation", "payment", "footer", "nav", "navbar", "breadcrumb", "cookie"},
		PreferWords: []string{"hero", "banner", "services", "service", "about", "story", "team", "products",
			"product", "projects", "project", "portfolio", "work", "gallery", "blog", "article", "post",
			"news", "cta", "menu", "showcase", "field"},
		LogoWords: []string{"logo", "brandmark", "wordmark", "favicon", "icon", "sprite", "avatar", "badge",
			"emoji", "placeholder", "pixel", "spacer", "loader", "spinner", "flag"},
		MinSide:    400,
		ReadWindow: 40 * time.Second,
	}
}

// DefaultConfig is 3 to 5 images of at most 2 MB, at least 1024 px on the short side; the website
// sweep runs daily, reads a site at most weekly and 50 tenants per run.
func DefaultConfig() Config {
	return Config{Min: 3, Max: 5, MaxBytes: 2 << 20, MinShortSide: 1024,
		MaxScreens: 6, MinScreenSide: 600, ListLinkTTL: 15 * time.Minute, RenderLinkTTL: time.Hour,
		SiteSweepEvery: 24 * time.Hour, SiteRetryAfter: 7 * 24 * time.Hour, SiteSweepLimit: 50,
		Site: DefaultSiteRules()}
}

// ConfigFromEnv reads MF_BRAND_IMAGES_MIN, MF_BRAND_IMAGES_MAX, MF_BRAND_IMAGES_MAX_BYTES,
// MF_BRAND_IMAGES_MIN_SIDE, MF_BRAND_IMAGES_MAX_SCREENS, MF_BRAND_IMAGES_MIN_SCREEN_SIDE,
// MF_BRAND_IMAGES_LINK_TTL, MF_BRAND_IMAGES_RENDER_LINK_TTL,
// MF_BRAND_IMAGES_SITE_SWEEP_EVERY, MF_BRAND_IMAGES_SITE_RETRY_AFTER and
// MF_BRAND_IMAGES_SITE_SWEEP_LIMIT, and for choosing website pictures MF_BRAND_IMAGES_SITE_PAGES,
// MF_BRAND_IMAGES_SITE_EXCLUDE_WORDS, MF_BRAND_IMAGES_SITE_PREFER_WORDS and
// MF_BRAND_IMAGES_SITE_LOGO_WORDS (comma separated lists that replace the defaults),
// MF_BRAND_IMAGES_SITE_MAX_PAGES, MF_BRAND_IMAGES_SITE_MIN_DISPLAY and
// MF_BRAND_IMAGES_SITE_READ_WINDOW. An unset value keeps its default; a value that does not
// parse, or limits that contradict each other, return the defaults with an error for the caller
// to log.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := DefaultConfig()
	lists := []struct {
		key string
		dst *[]string
	}{
		{"MF_BRAND_IMAGES_SITE_PAGES", &cfg.Site.Pages},
		{"MF_BRAND_IMAGES_SITE_EXCLUDE_WORDS", &cfg.Site.ExcludeWords},
		{"MF_BRAND_IMAGES_SITE_PREFER_WORDS", &cfg.Site.PreferWords},
		{"MF_BRAND_IMAGES_SITE_LOGO_WORDS", &cfg.Site.LogoWords},
	}
	for _, it := range lists {
		if v := strings.TrimSpace(getenv(it.key)); v != "" {
			var words []string
			for _, w := range strings.Split(v, ",") {
				if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
					words = append(words, w)
				}
			}
			*it.dst = words
		}
	}
	ints := []struct {
		key string
		dst *int
	}{
		{"MF_BRAND_IMAGES_MIN", &cfg.Min},
		{"MF_BRAND_IMAGES_MAX", &cfg.Max},
		{"MF_BRAND_IMAGES_MIN_SIDE", &cfg.MinShortSide},
		{"MF_BRAND_IMAGES_MAX_SCREENS", &cfg.MaxScreens},
		{"MF_BRAND_IMAGES_MIN_SCREEN_SIDE", &cfg.MinScreenSide},
		{"MF_BRAND_IMAGES_SITE_SWEEP_LIMIT", &cfg.SiteSweepLimit},
		{"MF_BRAND_IMAGES_SITE_MAX_PAGES", &cfg.Site.MaxPages},
		{"MF_BRAND_IMAGES_SITE_MIN_DISPLAY", &cfg.Site.MinSide},
	}
	for _, it := range ints {
		if v := strings.TrimSpace(getenv(it.key)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return DefaultConfig(), fmt.Errorf("%s must be a positive whole number, got %q", it.key, v)
			}
			*it.dst = n
		}
	}
	if v := strings.TrimSpace(getenv("MF_BRAND_IMAGES_MAX_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return DefaultConfig(), fmt.Errorf("MF_BRAND_IMAGES_MAX_BYTES must be a positive number of bytes, got %q", v)
		}
		cfg.MaxBytes = n
	}
	durations := []struct {
		key string
		dst *time.Duration
	}{
		{"MF_BRAND_IMAGES_LINK_TTL", &cfg.ListLinkTTL},
		{"MF_BRAND_IMAGES_RENDER_LINK_TTL", &cfg.RenderLinkTTL},
		{"MF_BRAND_IMAGES_SITE_SWEEP_EVERY", &cfg.SiteSweepEvery},
		{"MF_BRAND_IMAGES_SITE_RETRY_AFTER", &cfg.SiteRetryAfter},
		{"MF_BRAND_IMAGES_SITE_READ_WINDOW", &cfg.Site.ReadWindow},
	}
	for _, it := range durations {
		if v := strings.TrimSpace(getenv(it.key)); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return DefaultConfig(), fmt.Errorf("%s must be a Go duration such as 15m, got %q", it.key, v)
			}
			*it.dst = d
		}
	}
	if cfg.Min > cfg.Max {
		return DefaultConfig(), fmt.Errorf("MF_BRAND_IMAGES_MIN (%d) is above MF_BRAND_IMAGES_MAX (%d)", cfg.Min, cfg.Max)
	}
	return cfg, nil
}

// register makes the media type match the configured size limit. JPEG and PNG only: the shared
// store re-encodes those two (which drops EXIF location and device data), while a WebP file would
// be stored as sent and its size could not be read with the standard library. The portal turns a
// WebP pick into a JPEG before it uploads.
func register(cfg Config) {
	objectstore.Register(objectstore.MediaType{
		Name:     MediaType,
		MaxBytes: cfg.MaxBytes,
		Allowed:  []string{"image/jpeg", "image/png"},
		Image:    true,
	})
}

func init() { register(DefaultConfig()) }
