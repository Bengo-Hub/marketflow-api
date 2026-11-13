package brandimages

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
)

func TestConfigDefaultsAndEnv(t *testing.T) {
	d := DefaultConfig()
	if d.Min != 3 || d.Max != 5 || d.MaxBytes != 2<<20 || d.MinShortSide != 1024 || d.ListLinkTTL != 15*time.Minute {
		t.Fatalf("defaults: %+v", d)
	}
	env := map[string]string{"MF_BRAND_IMAGES_MIN": "2", "MF_BRAND_IMAGES_MAX": "6", "MF_BRAND_IMAGES_MAX_BYTES": "3145728",
		"MF_BRAND_IMAGES_MIN_SIDE": "800", "MF_BRAND_IMAGES_LINK_TTL": "5m", "MF_BRAND_IMAGES_RENDER_LINK_TTL": "2h"}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Min != 2 || cfg.Max != 6 || cfg.MaxBytes != 3<<20 || cfg.MinShortSide != 800 ||
		cfg.ListLinkTTL != 5*time.Minute || cfg.RenderLinkTTL != 2*time.Hour {
		t.Fatalf("env: %+v", cfg)
	}
	for name, bad := range map[string]map[string]string{
		"not a number":  {"MF_BRAND_IMAGES_MAX": "five"},
		"zero":          {"MF_BRAND_IMAGES_MIN_SIDE": "0"},
		"bad duration":  {"MF_BRAND_IMAGES_LINK_TTL": "soon"},
		"min above max": {"MF_BRAND_IMAGES_MIN": "6", "MF_BRAND_IMAGES_MAX": "4"},
	} {
		got, err := ConfigFromEnv(func(k string) string { return bad[k] })
		if err == nil || got != DefaultConfig() {
			t.Errorf("%s: want the defaults and an error, got %+v %v", name, got, err)
		}
	}
}

func TestKeysStayInsideTheTenant(t *testing.T) {
	tid, other := uuid.New(), uuid.New()
	meta := map[string]any{MetadataKey: []any{
		map[string]any{"id": "a", "key": Prefix(tid) + "2026/11/a.jpg"},
		map[string]any{"id": "b", "key": Prefix(other) + "2026/11/b.jpg"},
		map[string]any{"id": "c", "key": Prefix(tid) + "2026/11/c.jpg"},
		map[string]any{"id": "d", "key": Prefix(tid) + "../" + other.String() + "/x.jpg"},
	}}
	got := Keys(meta, tid, 0)
	if len(got) != 2 || !strings.HasSuffix(got[0], "a.jpg") || !strings.HasSuffix(got[1], "c.jpg") {
		t.Fatalf("keys: %v", got)
	}
	if got := Keys(meta, tid, 1); len(got) != 1 {
		t.Fatalf("capped keys: %v", got)
	}
	l := NewLinker(nil, time.Minute)
	if links := l.Links(tid, got); len(links) != 0 {
		t.Fatalf("no store links nothing: %v", links)
	}
}

const testDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedTenant(t *testing.T, db *ent.Client) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("BI " + tid.String()).SetSlug("bi-" + tid.String()).
		SetMetadata(map[string]any{"keep": "me"}).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(ctx) })
	return tid
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x += 7 {
		img.Set(x, x%h, color.RGBA{R: 200, G: 80, B: 40, A: 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type harness struct {
	t      *testing.T
	router http.Handler
	root   string
}

func newHarness(t *testing.T, db *ent.Client, cfg Config) *harness {
	root := t.TempDir()
	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: root, LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(NewService(db, store, cfg, zap.NewNop()), zap.NewNop())
	r := chi.NewRouter()
	r.Get("/tenant/brand-images", h.List)
	r.Post("/tenant/brand-images", h.Upload)
	r.Delete("/tenant/brand-images/{id}", h.Delete)
	return &harness{t: t, router: r, root: root}
}

func (h *harness) upload(tid uuid.UUID, data []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "shop.jpg")
	_, _ = fw.Write(data)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/tenant/brand-images", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Tenant-ID", tid.String())
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *harness) do(method, path string, tid uuid.UUID) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Tenant-ID", tid.String())
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *harness) list(tid uuid.UUID) listReply {
	rec := h.do(http.MethodGet, "/tenant/brand-images", tid)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var out listReply
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// Uploads are checked (type, size, resolution), capped at the maximum, private to the tenant,
// and deleted with their file.
func TestUploadListDelete(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid, other := seedTenant(t, db), seedTenant(t, db)
	cfg := DefaultConfig()
	cfg.Max, cfg.MinShortSide = 3, 64
	h := newHarness(t, db, cfg)

	if rec := h.upload(tid, []byte("plain text, not a picture")); rec.Code != http.StatusBadRequest {
		t.Fatalf("not an image: %d", rec.Code)
	}
	if rec := h.upload(tid, jpegOf(t, 200, 40)); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "64 px") {
		t.Fatalf("too small: %d %s", rec.Code, rec.Body)
	}
	var first View
	for i := 0; i < 3; i++ {
		rec := h.upload(tid, jpegOf(t, 120, 80))
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %d: %d %s", i, rec.Code, rec.Body)
		}
		if i == 0 {
			_ = json.Unmarshal(rec.Body.Bytes(), &first)
		}
	}
	if rec := h.upload(tid, jpegOf(t, 120, 80)); rec.Code != http.StatusConflict {
		t.Fatalf("over the maximum: %d", rec.Code)
	}
	got := h.list(tid)
	if len(got.Images) != 3 || got.Min != 3 || got.Max != 3 || got.MinShortSide != 64 || got.Images[0].Width != 120 {
		t.Fatalf("list: %+v", got)
	}
	if strings.Contains(h.do(http.MethodGet, "/tenant/brand-images", tid).Body.String(), `"key"`) {
		t.Fatal("the list must not expose storage keys")
	}

	// The keys sit under the tenant's prefix, and other metadata survives.
	ten, _ := db.Tenant.Get(ctx, tid)
	if ten.Metadata["keep"] != "me" {
		t.Fatal("other metadata was lost")
	}
	stored := Read(ten.Metadata)
	for _, img := range stored {
		if !Owned(img.Key, tid) {
			t.Fatalf("key outside the tenant: %s", img.Key)
		}
	}

	// Another tenant sees nothing and cannot delete this tenant's image.
	if l := h.list(other); len(l.Images) != 0 {
		t.Fatalf("other tenant's list: %+v", l.Images)
	}
	if rec := h.do(http.MethodDelete, "/tenant/brand-images/"+first.ID, other); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete: %d", rec.Code)
	}
	if rec := h.do(http.MethodDelete, "/tenant/brand-images/"+first.ID, tid); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodDelete, "/tenant/brand-images/"+first.ID, tid); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(h.root, filepath.FromSlash(stored[0].Key))); !os.IsNotExist(err) {
		t.Fatalf("deleted image's file still on disk: %v", err)
	}
	if l := h.list(tid); len(l.Images) != 2 {
		t.Fatalf("after delete: %d", len(l.Images))
	}
}

// Two uploads at once never pass the maximum.
func TestConcurrentUploadsKeepTheCap(t *testing.T) {
	db := openDB(t)
	tid := seedTenant(t, db)
	cfg := DefaultConfig()
	cfg.Max, cfg.MinShortSide = 2, 64
	h := newHarness(t, db, cfg)
	data := jpegOf(t, 100, 100)
	codes := make(chan int, 6)
	for i := 0; i < 6; i++ {
		go func() { codes <- h.upload(tid, data).Code }()
	}
	created := 0
	for i := 0; i < 6; i++ {
		if <-codes == http.StatusCreated {
			created++
		}
	}
	if created != 2 || len(h.list(tid).Images) != 2 {
		t.Fatalf("created %d, listed %d", created, len(h.list(tid).Images))
	}
}

// A private link expires: the remote store signs it with the configured lifetime.
func TestLinksArePresignedAndShortLived(t *testing.T) {
	store, err := objectstore.New("marketflow", objectstore.Config{Endpoint: "https://r2.example.com", AccessKeyID: "test-access-key",
		SecretAccessKey: "test-secret-not-real", PublicBucket: "pub", PrivateBucket: "priv", Region: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	tid := uuid.New()
	l := NewLinker(store, 10*time.Minute)
	links := l.Links(tid, []string{Prefix(tid) + "2026/11/a.jpg", Prefix(uuid.New()) + "2026/11/b.jpg"})
	if len(links) != 1 || !strings.Contains(links[0], "X-Amz-Expires=600") || !strings.Contains(links[0], "/priv/") {
		t.Fatalf("links: %v", links)
	}
}
