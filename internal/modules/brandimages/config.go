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
	// ListLinkTTL is the lifetime of the links the portal shows.
	ListLinkTTL time.Duration
	// RenderLinkTTL is the lifetime of the links sent to marketflow-ai with a render, long enough
	// for a queued video job to fetch them.
	RenderLinkTTL time.Duration
}

// DefaultConfig is 3 to 5 images of at most 2 MB, at least 1024 px on the short side.
func DefaultConfig() Config {
	return Config{Min: 3, Max: 5, MaxBytes: 2 << 20, MinShortSide: 1024,
		ListLinkTTL: 15 * time.Minute, RenderLinkTTL: time.Hour}
}

// ConfigFromEnv reads MF_BRAND_IMAGES_MIN, MF_BRAND_IMAGES_MAX, MF_BRAND_IMAGES_MAX_BYTES,
// MF_BRAND_IMAGES_MIN_SIDE, MF_BRAND_IMAGES_LINK_TTL and MF_BRAND_IMAGES_RENDER_LINK_TTL. An unset
// value keeps its default; a value that does not parse, or limits that contradict each other,
// return the defaults with an error for the caller to log.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := DefaultConfig()
	ints := []struct {
		key string
		dst *int
	}{
		{"MF_BRAND_IMAGES_MIN", &cfg.Min},
		{"MF_BRAND_IMAGES_MAX", &cfg.Max},
		{"MF_BRAND_IMAGES_MIN_SIDE", &cfg.MinShortSide},
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
