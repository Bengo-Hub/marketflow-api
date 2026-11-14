package brandimages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"

	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/platform/publicweb"
)

// Importing brand images from the business's own website (owner, 2026-11-14): a tenant that
// already shows pictures of itself on its site (codevertex-website for the platform owner) gets
// those linked first, and uploads only what is still missing. Only the tenant's own website and
// storefront are read, over public https; every picture passes the same checks as an upload (JPEG
// or PNG, size cap, minimum short side) and is stored as the tenant's own private object.

// ImportMarkerKey records the last website import in Tenant.metadata, so the sweep does not read
// the same site again every day.
const ImportMarkerKey = "brand_images_site_import"

// SiteLookup returns the addresses of the tenant's own website and storefront, best first. Empty
// means the tenant has none.
type SiteLookup func(ctx context.Context, tenantID uuid.UUID) ([]string, error)

// ErrNoSite is a tenant with no website or storefront on its account.
var ErrNoSite = errors.New("no website on the account")

// ImportResult is what one website import did.
type ImportResult struct {
	Sites   []string       `json:"sites"`
	Found   int            `json:"found"`
	Added   []View         `json:"added"`
	Skipped map[string]int `json:"skipped"`
	Total   int            `json:"total"`
	Min     int            `json:"min"`
}

// importMarker is the value kept under ImportMarkerKey.
type importMarker struct {
	At    string `json:"at"`
	Added int    `json:"added"`
}

const (
	maxSitePage      = 1536 << 10 // bytes of HTML read from a page
	maxCandidates    = 30         // pictures tried per import
	importReadWindow = 90 * time.Second
)

// WithSiteImport turns website imports on. The client defaults to publicweb's public-only client.
func (s *Service) WithSiteImport(lookup SiteLookup) *Service {
	s.siteLookup = lookup
	if s.web == nil {
		s.web = publicweb.Client(15*time.Second, 3)
	}
	return s
}

// SiteImportEnabled reports whether website imports are configured.
func (s *Service) SiteImportEnabled() bool { return s.siteLookup != nil }

// ImportFromSite links pictures from the tenant's own website and storefront until the tenant has
// its maximum. Pictures already imported (same address) are not taken twice.
func (s *Service) ImportFromSite(ctx context.Context, tenantID uuid.UUID) (ImportResult, error) {
	res := ImportResult{Skipped: map[string]int{}, Added: []View{}, Min: s.cfg.Min}
	if s.siteLookup == nil {
		return res, ErrNoSite
	}
	if !s.store.Enabled() {
		return res, ErrUnavailable
	}
	sites, err := s.siteLookup(ctx, tenantID)
	if err != nil {
		return res, err
	}
	for _, site := range sites {
		if u := normaliseHTTPS(site, nil); u != "" {
			res.Sites = append(res.Sites, u)
		}
	}
	if len(res.Sites) == 0 {
		return res, ErrNoSite
	}
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return res, err
	}
	have := Read(t.Metadata)
	res.Total = len(have)
	seen := map[string]bool{}
	for _, img := range have {
		if img.Source != "" {
			seen[img.Source] = true
		}
	}

	ctx, cancel := context.WithTimeout(ctx, importReadWindow)
	defer cancel()
	var candidates []string
	for _, site := range res.Sites {
		page, err := s.fetchPage(ctx, site)
		if err != nil {
			res.Skipped["site_unreadable"]++
			s.logger.Info("brand images: website not read", zap.String("site", site), zap.Error(err))
			continue
		}
		for _, c := range ImageCandidates(page, site) {
			if !seen[c] {
				seen[c] = true
				candidates = append(candidates, c)
			}
		}
	}
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}
	res.Found = len(candidates)
	// Pictures the business already has, uploaded by hand or linked before, by how they look.
	known := s.existingHashes(ctx, have)

	for _, c := range candidates {
		if res.Total >= s.cfg.Max {
			break
		}
		raw, err := s.fetchImage(ctx, c)
		if err != nil {
			res.Skipped[skipReason(err)]++
			continue
		}
		// Resized to what an upload must be: slightly small pictures are enlarged, WebP and
		// oversized ones re-encoded as JPEG under the cap.
		raw, err = s.fitPicture(raw)
		if errors.Is(err, errFarTooSmall) {
			res.Skipped["too_small"]++
			continue
		}
		if err != nil {
			res.Skipped["unsupported"]++
			continue
		}
		h, hashed := dHash(raw)
		if hashed && nearAny(h, known) {
			res.Skipped["duplicate"]++ // the same picture is already among the brand images
			continue
		}
		up, err := objectstore.Prepare(raw, MediaType)
		if err != nil {
			res.Skipped["unsupported"]++
			continue
		}
		up.FileName = path.Base(strings.SplitN(c, "?", 2)[0])
		v, err := s.add(ctx, tenantID, up, c)
		switch {
		case errors.Is(err, ErrFull):
			res.Total = s.cfg.Max
		case errors.Is(err, ErrTooSmall):
			res.Skipped["too_small"]++
		case err != nil:
			return res, err
		default:
			res.Added = append(res.Added, v)
			res.Total++
			if hashed {
				known = append(known, h) // the same picture twice on the site is linked once
			}
		}
	}
	s.markImported(ctx, tenantID, len(res.Added))
	return res, nil
}

func skipReason(err error) string {
	if errors.Is(err, errTooLarge) {
		return "too_large"
	}
	return "unreadable"
}

var errTooLarge = errors.New("picture too large to read")

// maxFetchBytes is the most read of one website picture before resizing: five times the stored
// cap, at least 10 MB.
func maxFetchBytes(capBytes int64) int64 { return max(capBytes*5, 10<<20) }

func (s *Service) fetchPage(ctx context.Context, site string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, site, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "MarketFlowBot/1.0 (brand pictures for the business's own ads)")
	req.Header.Set("Accept", "text/html")
	resp, err := s.web.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "html") {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSitePage))
	return string(b), err
}

func (s *Service) fetchImage(ctx context.Context, link string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MarketFlowBot/1.0 (brand pictures for the business's own ads)")
	req.Header.Set("Accept", "image/jpeg,image/png,image/webp")
	resp, err := s.web.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	// A large original is fine: it is resized and re-encoded under the cap (fitPicture).
	limit := maxFetchBytes(s.cfg.MaxBytes)
	if resp.ContentLength > limit {
		return nil, errTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}

// markImported records the import time, so the sweep waits before reading the site again.
func (s *Service) markImported(ctx context.Context, tenantID uuid.UUID, added int) {
	b, _ := json.Marshal(importMarker{At: s.nowFn().UTC().Format(time.RFC3339), Added: added})
	col := enttenant.FieldMetadata
	_, err := s.db.Tenant.Update().Where(enttenant.ID(tenantID)).
		Modify(func(u *sql.UpdateBuilder) {
			u.Set(col, sql.ExprFunc(func(b2 *sql.Builder) {
				b2.WriteString("jsonb_set(COALESCE(" + col + ", '{}'::jsonb), '{" + ImportMarkerKey + "}', ")
				b2.Arg(string(b))
				b2.WriteString("::jsonb)")
			}))
		}).Save(ctx)
	if err != nil {
		s.logger.Warn("brand images: import marker not saved", zap.String("tenant", tenantID.String()), zap.Error(err))
	}
}

// LastImport returns when the tenant's website was last read for pictures (zero: never).
func LastImport(meta map[string]any) time.Time {
	raw, ok := meta[ImportMarkerKey]
	if !ok {
		return time.Time{}
	}
	b, _ := json.Marshal(raw)
	var m importMarker
	_ = json.Unmarshal(b, &m)
	t, _ := time.Parse(time.RFC3339, m.At)
	return t
}

// SweepSiteImports imports website pictures for tenants that are below the minimum and whose site
// was not read in the last `again`. At most `limit` tenants per call; best effort, logged.
func (s *Service) SweepSiteImports(ctx context.Context, again time.Duration, limit int) {
	if !s.store.Enabled() {
		return
	}
	s.retryOrphans(ctx) // files whose delete failed earlier
	if s.siteLookup == nil {
		return
	}
	tenants, err := s.db.Tenant.Query().Where(enttenant.Status("active")).All(ctx)
	if err != nil {
		s.logger.Warn("brand images sweep: tenants not listed", zap.Error(err))
		return
	}
	now := s.nowFn()
	done := 0
	for _, t := range tenants {
		if done >= limit || ctx.Err() != nil {
			return
		}
		if len(Read(t.Metadata)) >= s.cfg.Min {
			continue
		}
		if last := LastImport(t.Metadata); !last.IsZero() && now.Sub(last) < again {
			continue
		}
		done++
		res, err := s.ImportFromSite(ctx, t.ID)
		if errors.Is(err, ErrNoSite) {
			s.markImported(ctx, t.ID, 0)
			continue
		}
		if err != nil {
			s.logger.Warn("brand images sweep: import failed", zap.String("tenant", t.Slug), zap.Error(err))
			continue
		}
		s.logger.Info("brand images sweep: website pictures linked", zap.String("tenant", t.Slug),
			zap.Int("added", len(res.Added)), zap.Int("total", res.Total), zap.Any("skipped", res.Skipped))
	}
}

// Picture discovery on a page. Patterns are deliberately simple: pages are the business's own,
// read once in a while, and anything odd is simply skipped.
var (
	reTag       = regexp.MustCompile(`(?is)<(meta|link|img|source)\b[^>]*>`)
	reAttr      = regexp.MustCompile(`(?is)([a-z:_-]+)\s*=\s*("([^"]*)"|'([^']*)')`)
	reLDImage   = regexp.MustCompile(`(?is)"image"\s*:\s*(\[[^\]]*\]|"[^"]+"|\{[^}]*\})`)
	reURLInJSON = regexp.MustCompile(`"(https?:[^"\s]+)"`)
	reSkipName  = regexp.MustCompile(`(?i)(favicon|sprite|icon|logo|avatar|placeholder|pixel|spacer|badge|emoji|loader|spinner)`)
)

// ImageCandidates returns the picture addresses a page offers, best first: the share images
// (og:image, twitter:image, image_src), then structured data, then the page's own pictures in
// order (the largest srcset entry of each). Logos, icons, SVG and GIF are left out; WebP is
// kept and turned into JPEG on import.
func ImageCandidates(page, pageURL string) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	var share, ld, imgs []string
	for _, tag := range reTag.FindAllString(page, -1) {
		attrs := map[string]string{}
		for _, m := range reAttr.FindAllStringSubmatch(tag, -1) {
			v := m[3]
			if v == "" {
				v = m[4]
			}
			attrs[strings.ToLower(m[1])] = strings.TrimSpace(v)
		}
		name := strings.ToLower(tag[1:5])
		switch {
		case strings.HasPrefix(name, "meta"):
			key := strings.ToLower(attrs["property"] + attrs["name"])
			if key == "og:image" || key == "og:image:url" || key == "og:image:secure_url" || key == "twitter:image" {
				share = append(share, attrs["content"])
			}
		case strings.HasPrefix(name, "link"):
			if strings.EqualFold(attrs["rel"], "image_src") {
				share = append(share, attrs["href"])
			}
		default: // img, source
			if best := largestSrcset(attrs["srcset"]); best != "" {
				imgs = append(imgs, best)
			} else if best := largestSrcset(attrs["data-srcset"]); best != "" {
				imgs = append(imgs, best)
			}
			for _, k := range []string{"src", "data-src", "data-lazy-src"} {
				if attrs[k] != "" {
					imgs = append(imgs, attrs[k])
					break
				}
			}
		}
	}
	for _, m := range reLDImage.FindAllStringSubmatch(page, -1) {
		for _, u := range reURLInJSON.FindAllStringSubmatch(m[1], -1) {
			ld = append(ld, strings.ReplaceAll(u[1], `\/`, "/"))
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, group := range [][]string{share, ld, imgs} {
		for _, raw := range group {
			u := normaliseHTTPS(raw, base)
			if u == "" || seen[u] || !pictureLike(u) {
				continue
			}
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// largestSrcset returns the widest entry of a srcset ("a.jpg 480w, b.jpg 1200w").
func largestSrcset(srcset string) string {
	type entry struct {
		url string
		w   float64
	}
	var list []entry
	for _, part := range strings.Split(srcset, ",") {
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) == 0 {
			continue
		}
		w := 1.0
		if len(f) > 1 {
			d := strings.ToLower(f[1])
			if n, err := strconv.ParseFloat(strings.TrimRight(d, "wx"), 64); err == nil {
				w = n
				if strings.HasSuffix(d, "x") {
					w *= 1000
				}
			}
		}
		list = append(list, entry{f[0], w})
	}
	if len(list) == 0 {
		return ""
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].w > list[j].w })
	return list[0].url
}

// normaliseHTTPS resolves raw against base and returns an https address, or "" when it is not one
// (data URIs, other schemes, empty). http is upgraded to https.
func normaliseHTTPS(raw string, base *url.URL) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(strings.ToLower(raw), "data:") {
		return ""
	}
	if base == nil && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	if u.Scheme != "https" || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

// pictureLike drops addresses that are clearly not photos of the business.
func pictureLike(u string) bool {
	p := strings.ToLower(strings.SplitN(u, "?", 2)[0])
	for _, ext := range []string{".svg", ".gif", ".ico", ".avif", ".bmp"} {
		if strings.HasSuffix(p, ext) {
			return false
		}
	}
	return !reSkipName.MatchString(path.Base(p))
}
