package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
)

// WebsiteCatalog reads codevertex-website's public catalogue (/api/catalog) and courses
// (/api/courses). The website owns both; onboarding only snapshots them into a draft.
type WebsiteCatalog struct {
	baseURL string
	http    *http.Client
	cache   *sharedcache.Aside
}

// websiteCacheTTL: the catalogue is site config and changes with deploys, not by the minute.
const websiteCacheTTL = 15 * time.Minute

// NewWebsiteCatalog builds the reader; an empty base URL turns it off. cache may be nil.
func NewWebsiteCatalog(baseURL string, cache *sharedcache.Aside) *WebsiteCatalog {
	return &WebsiteCatalog{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 2500 * time.Millisecond},
		cache:   cache,
	}
}

// WebsiteProduct is a Power Suite product or a service line.
type WebsiteProduct struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Tag         string   `json:"tag,omitempty"`
	Tagline     string   `json:"tagline,omitempty"`
	Description string   `json:"description"`
	URL         string   `json:"url,omitempty"`
	Status      string   `json:"status,omitempty"`
	Highlights  []string `json:"highlights,omitempty"`
	Features    []string `json:"features,omitempty"`
}

// WebsiteCourse is a Digitika course as /api/courses returns it.
type WebsiteCourse struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Duration    string `json:"duration"`
	Mode        string `json:"mode"`
	Price       int    `json:"price"`
	Description string `json:"description"`
	CoverImage  string `json:"coverImage"`
}

// Catalog is everything the website offers.
type Catalog struct {
	Products []WebsiteProduct `json:"products"`
	Services []WebsiteProduct `json:"services"`
	Courses  []WebsiteCourse  `json:"courses"`
}

// Load reads the catalogue and courses, cached for websiteCacheTTL. A failed courses call
// still returns the products, and the other way round.
func (w *WebsiteCatalog) Load(ctx context.Context) (Catalog, error) {
	if w == nil || w.baseURL == "" {
		return Catalog{}, fmt.Errorf("website catalogue not configured")
	}
	load := func(ctx context.Context) (Catalog, error) {
		var c Catalog
		errCat := w.get(ctx, "/api/catalog", &c)
		var courses []WebsiteCourse
		errCourses := w.get(ctx, "/api/courses", &courses)
		c.Courses = courses
		if errCat != nil && errCourses != nil {
			return Catalog{}, fmt.Errorf("website catalogue: %w", errCat)
		}
		return c, nil
	}
	if w.cache == nil {
		return load(ctx)
	}
	return sharedcache.GetOrSet(ctx, w.cache, sharedcache.Key("mf", "onboarding", "website-catalog"), websiteCacheTTL, load)
}

func (w *WebsiteCatalog) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", path, resp.StatusCode)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 2<<20)).Decode(out)
}
