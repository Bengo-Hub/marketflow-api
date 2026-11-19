package brandimages

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The fixtures are a software house's site in the shape of codevertexafrica.com on 2026-11-19: a
// hero with a small row of client logos, a "Trusted by" logo marquee, services and call to action
// photos, testimonials, a footer, and a blog page with a partner strip.

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var acmeNames = Names{Own: "Acme Software Limited",
	Others: []string{"The Urban Loft", "Small Steps Cosmetics", "Gram Auto Spares", "Acme Software Limited"}}

func pickURLs(picks []Pick) []string {
	out := make([]string, 0, len(picks))
	for _, p := range picks {
		out = append(out, p.URL)
	}
	return out
}

func TestPagePicturesLeavesOutClientLogosAndTakesContentPhotos(t *testing.T) {
	picks, skipped := PagePictures(fixture(t, "saas_home.html"), "https://acme.example.com/", DefaultSiteRules(), acmeNames)
	want := []string{
		"https://acme.example.com/images/photos/anpr-1600.jpg",
		"https://acme.example.com/_next/image?url=%2Fimages%2Fphotos%2Fteam-hackathon.webp&w=1920&q=75",
		"https://acme.example.com/images/photos/supermarket.jpg",
	}
	if got := pickURLs(picks); !reflect.DeepEqual(got, want) {
		t.Fatalf("picks\n got %v\nwant %v\nskipped %v", got, want, skipped)
	}
	sections := []string{picks[0].Section, picks[1].Section, picks[2].Section}
	if !reflect.DeepEqual(sections, []string{"services", "hero", "cta"}) {
		t.Fatalf("sections %v", sections)
	}
	for _, p := range picks {
		for _, bad := range []string{"logos/brand", "logo", "clients", "urban-loft", "jane", "footer", "flags"} {
			if strings.Contains(p.URL, bad) {
				t.Fatalf("took %s", p.URL)
			}
		}
	}
	// Each reason was seen: logos (share card, header, hero row, icon), the marquee, the
	// testimonial, the footer and navigation, and a photo naming another business.
	for _, k := range []string{"logo", "not_the_business", "page_chrome", "other_business"} {
		if skipped[k] == 0 {
			t.Errorf("no %q skips: %v", k, skipped)
		}
	}
}

func TestPagePicturesBlogDedupesAndSkipsPartners(t *testing.T) {
	picks, _ := PagePictures(fixture(t, "saas_blog.html"), "https://acme.example.com/blog", DefaultSiteRules(), acmeNames)
	if got := pickURLs(picks); !reflect.DeepEqual(got, []string{"https://acme.example.com/images/blog/students-coding.jpg"}) {
		t.Fatalf("blog picks %v", got)
	}
	if picks[0].Page != "https://acme.example.com/blog" || FromLabel(picks[0].Page, picks[0].Section) != "Blog page, post" {
		t.Fatalf("where: %+v / %q", picks[0], FromLabel(picks[0].Page, picks[0].Section))
	}
}

// A marquee drawn by repeating its logos (no telling class or heading) is still a strip.
func TestPagePicturesRepeatedStrip(t *testing.T) {
	page := `<main><section><h2>Hello</h2><img src="/hero.jpg" width="1600" height="900" alt="Our shop on a busy day">
<div>` + strings.Repeat(`<img src="/x/acme.jpg" width="1600" height="900" alt="">`, 3) + `</div></section></main>`
	picks, skipped := PagePictures(page, "https://shop.example.com/", DefaultSiteRules(), Names{})
	if got := pickURLs(picks); !reflect.DeepEqual(got, []string{"https://shop.example.com/hero.jpg"}) || skipped["logo_strip"] != 3 {
		t.Fatalf("picks %v skipped %v", got, skipped)
	}
}

func TestPagePicturesRulesAreConfig(t *testing.T) {
	r := DefaultSiteRules()
	r.ExcludeWords = append(r.ExcludeWords, "cta")
	picks, _ := PagePictures(fixture(t, "saas_home.html"), "https://acme.example.com/", r, acmeNames)
	for _, p := range picks {
		if p.Section == "cta" {
			t.Fatalf("excluded section taken: %+v", p)
		}
	}
	cfg, err := ConfigFromEnv(func(k string) string {
		return map[string]string{"MF_BRAND_IMAGES_SITE_EXCLUDE_WORDS": "Clients, sponsor ", "MF_BRAND_IMAGES_SITE_MAX_PAGES": "2",
			"MF_BRAND_IMAGES_SITE_READ_WINDOW": "30s"}[k]
	})
	if err != nil || !reflect.DeepEqual(cfg.Site.ExcludeWords, []string{"clients", "sponsor"}) || cfg.Site.MaxPages != 2 || cfg.Site.ReadWindow.Seconds() != 30 {
		t.Fatalf("site rules from env: %+v, %v", cfg.Site, err)
	}
}

func TestInnerPages(t *testing.T) {
	got := InnerPages(fixture(t, "saas_home.html"), "https://acme.example.com/", DefaultSiteRules())
	want := []string{"https://acme.example.com/services", "https://acme.example.com/about", "https://acme.example.com/blog"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inner pages %v", got)
	}
}

func TestRobots(t *testing.T) {
	rules := parseRobots("User-agent: *\nDisallow: /admin\nDisallow: /blog\nAllow: /blog$\n\nUser-agent: OtherBot\nDisallow: /\n")
	cases := map[string]bool{"/": true, "/services": true, "/admin/x": false, "/blog": true, "/blog/post-1": false}
	for p, want := range cases {
		if got := robotsAllowPath(rules, p); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
	own := parseRobots("User-agent: *\nAllow: /\n\nUser-agent: MarketFlowBot\nDisallow: /\n")
	if robotsAllowPath(own, "/") {
		t.Error("a group for MarketFlowBot must win over *")
	}
}

func TestFromLabel(t *testing.T) {
	cases := map[[2]string]string{
		{"https://a.example.com/", "hero"}:             "Home page, hero",
		{"https://a.example.com/services", "services"}: "Services page",
		{"https://a.example.com/about-us", ""}:         "About us page",
		{"", "hero"}:                                   "",
	}
	for in, want := range cases {
		if got := FromLabel(in[0], in[1]); got != want {
			t.Errorf("FromLabel(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// patternPhoto is a picture that differs from the others by its pattern, so the duplicate check
// keeps each.
func patternPhoto(t *testing.T, w, h, kind int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var v int
			switch kind {
			case 0:
				v = x * 255 / w
			case 1:
				v = (x / (w / 5)) % 2 * 255
			case 2:
				v = ((x / (w / 8)) + (y / (h / 8))) % 2 * 255
			default:
				v = 255 - (x*255/w+y*255/h)/2
			}
			img.Set(x, y, color.RGBA{R: uint8(v), G: uint8(v / 2), B: uint8(255 - v), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestImportFromSiteTakesTheBusinessPhotos reads the fixture site end to end: home page, the
// services and blog pages it links, robots.txt keeping /about out. Only the business's own photos
// are stored, each with the page and part it came from; no client logo is.
func TestImportFromSiteTakesTheBusinessPhotos(t *testing.T) {
	db := openDB(t)
	tid := seedTenant(t, db)
	// Another business on MarketFlow: a photo on the site that names it is never taken.
	other := seedTenant(t, db)
	if err := db.Tenant.UpdateOneID(other).SetName("The Urban Loft").Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	home := fixture(t, "saas_home.html")
	blog := fixture(t, "saas_blog.html")
	pics := map[string][]byte{
		"/images/photos/anpr-1600.jpg":      patternPhoto(t, 1600, 1066, 0),
		"/images/photos/supermarket.jpg":    patternPhoto(t, 1600, 1066, 1),
		"/images/blog/students-coding.jpg":  patternPhoto(t, 1600, 1000, 2),
		"/images/photos/team-hackathon.jpg": patternPhoto(t, 1920, 1280, 3),
	}
	mux := http.NewServeMux()
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("/{$}", page(home))
	mux.HandleFunc("/services", page(`<main><section id="services"><h1>Services</h1></section></main>`))
	mux.HandleFunc("/blog", page(blog))
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) { t.Error("robots.txt said no to /about") })
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /about\n"))
	})
	mux.HandleFunc("/_next/image", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("w") != "1920" {
			http.Error(w, "w missing: the address was not unescaped", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(pics["/images/photos/team-hackathon.jpg"])
	})
	for p, data := range pics {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(data)
		})
	}
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, store, DefaultConfig(), zap.NewNop())
	svc.web = srv.Client()
	svc.WithSiteImport(func(ctx context.Context, id uuid.UUID) ([]string, error) { return []string{srv.URL + "/"}, nil })

	res, err := svc.ImportFromSite(context.Background(), tid)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(res.Added) != 4 {
		for _, v := range res.Added {
			t.Logf("added %s from %s", v.Source, v.From)
		}
		t.Fatalf("added %d of %d, skipped %v", len(res.Added), res.Found, res.Skipped)
	}
	froms := map[string]bool{}
	for _, v := range res.Added {
		froms[v.From] = true
		if strings.Contains(v.Source, "logo") || strings.Contains(v.Source, "example.net") {
			t.Fatalf("stored a logo: %s", v.Source)
		}
	}
	for _, f := range []string{"Home page, services", "Home page, hero", "Home page, cta", "Blog page, post"} {
		if !froms[f] {
			t.Errorf("no picture from %q in %v", f, froms)
		}
	}
	tn, _ := db.Tenant.Get(context.Background(), tid)
	for _, img := range Read(tn.Metadata) {
		if img.Page == "" || img.Name == "image" {
			t.Fatalf("stored without where it came from: %+v", img)
		}
	}
}

// A site that does not answer is an error the portal can explain, not an empty success.
func TestImportFromSiteUnreadable(t *testing.T) {
	db := openDB(t)
	tid := seedTenant(t, db)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(521)
	}))
	defer srv.Close()
	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, store, DefaultConfig(), zap.NewNop())
	svc.web = srv.Client()
	svc.WithSiteImport(func(ctx context.Context, id uuid.UUID) ([]string, error) { return []string{srv.URL + "/"}, nil })
	if _, err := svc.ImportFromSite(context.Background(), tid); err == nil || !strings.Contains(err.Error(), "521") {
		t.Fatalf("want ErrSiteUnreadable with the status, got %v", err)
	}
}
