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
	"sync"
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
//
// Since 2026-11-19 the home page and the site's own inner pages (services, about, products,
// projects, blog) are read, robots.txt is obeyed, and pictures are chosen by where they sit on the
// page (pick.go): a client logo marquee on a software house's site is never taken.

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
	maxSitePage   = 1536 << 10 // bytes of HTML read from a page
	maxCandidates = 30         // pictures tried per import
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
	res.Total = len(OfKind(t.Metadata, tenantID, KindPhoto))
	seen := map[string]bool{}
	for _, img := range have {
		if img.Source != "" {
			seen[img.Source] = true
		}
	}

	// Reading is bounded so the portal gets its answer before the gateway gives up; storing what
	// was read uses the caller's context.
	window := s.cfg.Site.ReadWindow
	if window <= 0 {
		window = DefaultSiteRules().ReadWindow
	}
	readCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	names := s.businessNames(ctx, t.ID, t.Name)
	picks, pagesRead, why := s.sitePictures(readCtx, res.Sites, names, res.Skipped)
	if pagesRead == 0 {
		return res, fmt.Errorf("%w: %s", ErrSiteUnreadable, why)
	}
	var candidates []Pick
	for _, p := range picks {
		if !seen[p.URL] {
			seen[p.URL] = true
			candidates = append(candidates, p)
		}
	}
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}
	res.Found = len(candidates)
	// Pictures the business already has, uploaded by hand or linked before, by how they look.
	known := s.existingHashes(ctx, have)

	// Pictures are downloaded a few at a time and stored best first.
	for start := 0; start < len(candidates) && res.Total < s.cfg.Max; start += imageBatch {
		batch := candidates[start:min(start+imageBatch, len(candidates))]
		raws := make([][]byte, len(batch))
		errs := make([]error, len(batch))
		var wg sync.WaitGroup
		for i, c := range batch {
			wg.Add(1)
			go func() {
				defer wg.Done()
				raws[i], errs[i] = s.fetchImage(readCtx, c.URL)
			}()
		}
		wg.Wait()
		for i, c := range batch {
			if res.Total >= s.cfg.Max {
				break
			}
			if err := s.storePick(ctx, tenantID, c, raws[i], errs[i], &res, &known); err != nil {
				return res, err
			}
		}
	}
	s.markImported(ctx, tenantID, len(res.Added))
	return res, nil
}

// imageBatch is how many website pictures are downloaded at once.
const imageBatch = 4

// ErrSiteUnreadable is a website none of whose pages could be read.
var ErrSiteUnreadable = errors.New("the website could not be read")

// sitePictures reads each site's home page and its inner pages (services, about, products,
// projects, blog...) that robots.txt allows, and returns the pictures that show the business, best
// first, with how many pages were read and, when none was, why.
func (s *Service) sitePictures(ctx context.Context, sites []string, names Names, skipped map[string]int) ([]Pick, int, string) {
	rules := s.cfg.Site
	type pageRead struct {
		url, body string
		err       error
	}
	var all []Pick
	read, why := 0, ""
	for _, site := range sites {
		allowed := s.robots(ctx, site)
		if !allowed(site) {
			skipped["robots"]++
			why = "robots.txt does not allow reading " + site
			continue
		}
		home, err := s.fetchPage(ctx, site)
		if err != nil {
			skipped["site_unreadable"]++
			why = site + ": " + err.Error()
			s.logger.Info("brand images: website not read", zap.String("site", site), zap.Error(err))
			continue
		}
		read++
		found, sk := PagePictures(home, site, rules, names)
		all = append(all, found...)
		addCounts(skipped, sk)

		var inner []string
		for _, p := range InnerPages(home, site, rules) {
			if allowed(p) {
				inner = append(inner, p)
			} else {
				skipped["robots"]++
			}
		}
		pages := make([]pageRead, len(inner))
		var wg sync.WaitGroup
		for i, p := range inner {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body, err := s.fetchPage(ctx, p)
				pages[i] = pageRead{p, body, err}
			}()
		}
		wg.Wait()
		for _, p := range pages {
			if p.err != nil {
				skipped["page_unreadable"]++
				continue
			}
			read++
			found, sk := PagePictures(p.body, p.url, rules, names)
			all = append(all, found...)
			addCounts(skipped, sk)
		}
	}
	return rank(all), read, why
}

func addCounts(dst, src map[string]int) {
	for k, v := range src {
		dst[k] += v
	}
}

// businessNames are the tenant's own name and every other tenant's, so a picture that names
// another business is never taken (owner, 2026-11-19: pictures are never mixed between tenants).
func (s *Service) businessNames(ctx context.Context, tenantID uuid.UUID, own string) Names {
	others, err := s.db.Tenant.Query().Where(enttenant.IDNEQ(tenantID)).Select(enttenant.FieldName).Strings(ctx)
	if err != nil {
		s.logger.Warn("brand images: other business names not read", zap.Error(err))
	}
	return Names{Own: own, Others: others}
}

// storePick checks and stores one downloaded picture. Only a cancelled caller is returned as an
// error; everything else is counted in res.Skipped.
func (s *Service) storePick(ctx context.Context, tenantID uuid.UUID, c Pick, raw []byte, err error, res *ImportResult, known *[]uint64) error {
	if err != nil {
		res.Skipped[skipReason(err)]++
		return nil
	}
	// A picture that is largely see-through is a logo or a cut-out mark, not a photo of the business.
	if mostlyTransparent(raw) {
		res.Skipped["logo"]++
		return nil
	}
	// Resized to what an upload must be: slightly small pictures are enlarged, WebP and oversized
	// ones re-encoded as JPEG under the cap.
	raw, err = s.fitPicture(raw)
	if errors.Is(err, errFarTooSmall) {
		res.Skipped["too_small"]++
		return nil
	}
	if err != nil {
		res.Skipped["unsupported"]++
		return nil
	}
	h, hashed := dHash(raw)
	if hashed && nearAny(h, *known) {
		res.Skipped["duplicate"]++ // the same picture is already among the brand images
		return nil
	}
	up, err := objectstore.Prepare(raw, MediaType)
	if err != nil {
		res.Skipped["unsupported"]++
		return nil
	}
	up.FileName = pictureFileName(c.URL)
	v, err := s.add(ctx, tenantID, up, origin{Source: c.URL, Page: c.Page, Section: c.Section})
	switch {
	case errors.Is(err, ErrFull):
		res.Total = s.cfg.Max
	case errors.Is(err, ErrTooSmall):
		res.Skipped["too_small"]++
	case err != nil:
		return err
	default:
		res.Added = append(res.Added, v)
		res.Total++
		if hashed {
			*known = append(*known, h) // the same picture twice on the site is linked once
		}
	}
	return nil
}

// pictureFileName is the file name a website picture is stored under: the real file behind an
// image proxy (/_next/image?url=/images/team.webp gives team.webp), else the last part of its path.
func pictureFileName(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return path.Base(strings.SplitN(link, "?", 2)[0])
	}
	return path.Base(innerPicture(u))
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
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
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
	req.Header.Set("User-Agent", userAgent)
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
		if len(OfKind(t.Metadata, t.ID, KindPhoto)) >= s.cfg.Min {
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

// Structured data images ("image": "..." or a list of them) inside JSON-LD.
var (
	reLDImage   = regexp.MustCompile(`(?is)"image"\s*:\s*(\[[^\]]*\]|"[^"]+"|\{[^}]*\})`)
	reURLInJSON = regexp.MustCompile(`"(https?:[^"\s]+)"`)
)

// ImageCandidates returns the addresses of the pictures on one page that show the business, best
// first, under the default rules and without business names (PagePictures has the details).
func ImageCandidates(page, pageURL string) []string {
	picks, _ := PagePictures(page, pageURL, DefaultSiteRules(), Names{})
	out := make([]string, 0, len(picks))
	for _, p := range picks {
		out = append(out, p.URL)
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

