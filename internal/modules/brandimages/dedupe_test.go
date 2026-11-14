package brandimages

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// photoOf is a picture with real structure (a diagonal gradient and a bright block), drawn at any
// size, so a resized copy is still the same picture.
func photoOf(t *testing.T, w, h int, flip bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x*255/w + y*255/h) / 2)
			if flip {
				v = 255 - v
			}
			if x > w/3 && x < w/2 && y > h/4 && y < h/2 {
				v = 250
			}
			img.Set(x, y, color.RGBA{R: v, G: v / 2, B: 255 - v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The same picture at another size hashes close; a different picture does not.
func TestDHashMatchesResizedCopies(t *testing.T) {
	a, ok1 := dHash(photoOf(t, 1200, 800, false))
	b, ok2 := dHash(photoOf(t, 1536, 1024, false))
	c, ok3 := dHash(photoOf(t, 1200, 800, true))
	if !ok1 || !ok2 || !ok3 {
		t.Fatal("hash failed")
	}
	if !nearAny(a, []uint64{b}) {
		t.Fatalf("resized copy not matched: %016x vs %016x", a, b)
	}
	if nearAny(a, []uint64{c}) {
		t.Fatalf("different picture matched: %016x vs %016x", a, c)
	}
	if _, ok := parseHash(formatHash(a)); !ok {
		t.Fatal("hash does not round-trip")
	}
}

// A website picture the business already uploaded is not linked again.
func TestImportSkipsPicturesAlreadyUploaded(t *testing.T) {
	db := openDB(t)
	tid := seedTenant(t, db)
	uploaded := photoOf(t, 1600, 1100, false) // the owner's own copy, a different size
	site := photoOf(t, 1200, 1100, false)
	other := photoOf(t, 1300, 1100, true)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<img src="/same.jpg"><img src="/other.jpg">`))
	})
	serve := func(data []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "image/jpeg"); _, _ = w.Write(data) }
	}
	mux.HandleFunc("/same.jpg", serve(site))
	mux.HandleFunc("/other.jpg", serve(other))
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, store, DefaultConfig(), zap.NewNop())
	svc.web = srv.Client()
	svc.WithSiteImport(func(ctx context.Context, id uuid.UUID) ([]string, error) { return []string{srv.URL + "/"}, nil })
	ctx := context.Background()
	up, err := objectstore.Prepare(uploaded, MediaType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, tid, up); err != nil {
		t.Fatalf("upload: %v", err)
	}
	res, err := svc.ImportFromSite(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Skipped["duplicate"] != 1 || res.Total != 2 {
		t.Fatalf("added %d, skipped %v, total %d", len(res.Added), res.Skipped, res.Total)
	}
}
