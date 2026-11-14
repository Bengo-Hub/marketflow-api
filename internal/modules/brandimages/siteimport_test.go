package brandimages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestImageCandidatesOrderAndFilters(t *testing.T) {
	page := `<html><head>
<meta property="og:image" content="/media/hero.jpg">
<meta name="twitter:image" content="https://cdn.example.com/share.png?v=2">
<link rel="icon" href="/favicon.ico">
<script type="application/ld+json">{"@type":"Store","image":["https:\/\/cdn.example.com\/shop-front.jpg"]}</script>
</head><body>
<img src="/img/logo.png" alt="logo">
<img src="/img/team.jpg" srcset="/img/team-480.jpg 480w, /img/team-1600.jpg 1600w">
<img data-src="http://example.com/img/products.jpeg">
<img src="data:image/png;base64,AAAA">
<img src="/img/banner.svg">
<img src="/media/hero.jpg">
</body></html>`
	got := ImageCandidates(page, "https://example.com/about")
	want := []string{
		"https://example.com/media/hero.jpg",
		"https://cdn.example.com/share.png?v=2",
		"https://cdn.example.com/shop-front.jpg",
		"https://example.com/img/team-1600.jpg",
		"https://example.com/img/team.jpg",
		"https://example.com/img/products.jpeg",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates\n got %v\nwant %v", got, want)
	}
}

func TestLargestSrcsetAndNormalise(t *testing.T) {
	if got := largestSrcset("a.jpg 1x, b.jpg 2x"); got != "b.jpg" {
		t.Fatalf("density srcset: %q", got)
	}
	if got := largestSrcset(""); got != "" {
		t.Fatalf("empty srcset: %q", got)
	}
	if got := normaliseHTTPS("codevertexafrica.com", nil); got != "https://codevertexafrica.com" {
		t.Fatalf("bare host: %q", got)
	}
	if got := normaliseHTTPS("ftp://x.example.com/a.jpg", nil); got != "" {
		t.Fatalf("ftp kept: %q", got)
	}
}

func TestLastImportRoundTrip(t *testing.T) {
	if !LastImport(map[string]any{}).IsZero() {
		t.Fatal("no marker should be zero")
	}
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	meta := map[string]any{ImportMarkerKey: map[string]any{"at": at.Format(time.RFC3339), "added": 2}}
	if got := LastImport(meta); !got.Equal(at) {
		t.Fatalf("marker: %v", got)
	}
}

// A slightly small picture is enlarged to the minimum, a far too small one refused, a large one
// reduced, and one that fits kept as sent (owner, 2026-11-14: resize where it makes sense).
func TestFitSize(t *testing.T) {
	cases := []struct {
		w, h, wantW, wantH int
		resized            bool
		err                bool
	}{
		{1200, 800, 1536, 1024, true, false},
		{800, 1200, 1024, 1536, true, false},
		{600, 400, 600, 400, false, true},
		{1600, 1200, 1600, 1200, false, false},
		{6000, 4000, 2560, 1707, true, false},
	}
	for _, c := range cases {
		w, h, resized, err := fitSize(c.w, c.h, 1024)
		if (err != nil) != c.err || resized != c.resized || (!c.err && (w != c.wantW || h != c.wantH)) {
			t.Errorf("fitSize(%d, %d) = %d, %d, %v, %v", c.w, c.h, w, h, resized, err)
		}
	}
}

// TestImportFromSite links the big pictures of a site, enlarges the slightly small one, skips the
// logo, and does not take the same pictures twice.
func TestImportFromSite(t *testing.T) {
	db := openDB(t)
	tid := seedTenant(t, db)
	big := jpegOf(t, 1200, 1100)
	small := jpegOf(t, 400, 400) // more than 2x below the minimum: refused, never enlarged
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<meta property="og:image" content="/a.jpg"><img src="/logo.jpg"><img src="/b.jpg"><img src="/small.jpg">`))
	})
	for _, p := range []string{"/a.jpg", "/b.jpg", "/logo.jpg"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(big)
		})
	}
	mux.HandleFunc("/small.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(small)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, store, DefaultConfig(), zap.NewNop())
	svc.web = srv.Client() // the test server is on loopback, which the public-only client refuses
	svc.WithSiteImport(func(ctx context.Context, id uuid.UUID) ([]string, error) {
		return []string{srv.URL + "/"}, nil
	})

	ctx := context.Background()
	res, err := svc.ImportFromSite(ctx, tid)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(res.Added) != 2 || res.Skipped["too_small"] != 1 || res.Total != 2 {
		t.Fatalf("first import: added %d, skipped %v, total %d", len(res.Added), res.Skipped, res.Total)
	}
	again, err := svc.ImportFromSite(ctx, tid)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if len(again.Added) != 0 || again.Total != 2 {
		t.Fatalf("second import took pictures again: added %d, total %d", len(again.Added), again.Total)
	}
	tn, err := db.Tenant.Get(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if LastImport(tn.Metadata).IsZero() || tn.Metadata["keep"] != "me" {
		t.Fatalf("metadata after import: %v", tn.Metadata)
	}
	for _, img := range Read(tn.Metadata) {
		if img.Source == "" {
			t.Fatalf("imported picture without its source: %+v", img)
		}
	}
}

func TestImportWithoutSite(t *testing.T) {
	db := openDB(t)
	tid := seedTenant(t, db)
	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, store, DefaultConfig(), zap.NewNop()).
		WithSiteImport(func(ctx context.Context, id uuid.UUID) ([]string, error) { return nil, nil })
	if _, err := svc.ImportFromSite(context.Background(), tid); err != ErrNoSite {
		t.Fatalf("want ErrNoSite, got %v", err)
	}
}
