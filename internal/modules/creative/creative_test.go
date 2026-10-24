package creative

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
)

const testPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
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

// newTenant returns a fresh tenant id and removes its creative rows afterwards.
// The creative tables carry tenant_id without a foreign key, so no tenant row is needed.
func newTenant(t *testing.T, db *ent.Client) uuid.UUID {
	tid := uuid.New()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.CreativeAsset.Delete().Where(creativeasset.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
	})
	return tid
}

func ptr[T any](v T) *T { return &v }

func newBrief(t *testing.T, s *Service, tid uuid.UUID) *ent.CreativeBrief {
	t.Helper()
	b, err := s.CreateBrief(context.Background(), tid, nil, BriefInput{
		Title:    ptr("Shea butter launch"),
		Offering: map[string]any{"name": "Shea butter 250g", "photo_urls": []any{"https://example.com/shea.jpg"}},
		Metadata: map[string]any{"brand": map[string]any{"accent": "#1f7a4d", "logo_url": "https://example.com/logo.png", "palette": []any{"#1f7a4d", "#f4e9d8"}}},
	})
	if err != nil {
		t.Fatalf("create brief: %v", err)
	}
	return b
}

func TestBriefValidation(t *testing.T) {
	s := NewService(openDB(t), zap.NewNop())
	tid := newTenant(t, s.db)
	ctx := context.Background()
	cases := []BriefInput{
		{},
		{Title: ptr("  ")},
		{Title: ptr("ok"), Objective: ptr("fame")},
		{Title: ptr("ok"), Language: ptr("fr")},
	}
	for i, in := range cases {
		if _, err := s.CreateBrief(ctx, tid, nil, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d: want ErrInvalid, got %v", i, err)
		}
	}
	b := newBrief(t, s, tid)
	if b.Objective != "sales" || b.Language != "en" || b.Status != "active" {
		t.Fatalf("defaults: %+v", b)
	}
	if _, err := s.UpdateBrief(ctx, uuid.New(), b.ID, BriefInput{Title: ptr("x")}); !ent.IsNotFound(err) {
		t.Fatalf("another tenant must not see the brief: %v", err)
	}
	rows, total, err := s.ListBriefs(ctx, tid, "", 20, 0)
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("list: %d %d %v", total, len(rows), err)
	}
}

func TestVersionNumbersAndReview(t *testing.T) {
	s := NewService(openDB(t), zap.NewNop())
	tid := newTenant(t, s.db)
	ctx := context.Background()
	b := newBrief(t, s, tid)

	if _, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "x", Format: "800x600"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad format: %v", err)
	}
	if _, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "x", PhotoURLs: []string{"file:///etc/passwd"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad photo url: %v", err)
	}

	// Concurrent requests still get distinct numbers.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "studio shot on a wooden table", Headline: "Soft skin"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("create version: %v", err)
		}
	}
	vs, _ := s.db.CreativeVersion.Query().Where(creativeversion.BriefID(b.ID)).Order(ent.Asc(creativeversion.FieldVersionNo)).All(ctx)
	if len(vs) != 4 || vs[0].VersionNo != 1 || vs[3].VersionNo != 4 {
		t.Fatalf("version numbers: %d versions", len(vs))
	}
	v := vs[0]
	if v.Status != StatusQueued || v.Format != "1080x1350" || v.Tier != "standard" {
		t.Fatalf("version defaults: %+v", v)
	}

	// A queued version cannot be approved.
	if _, err := s.Approve(ctx, tid, v.ID, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve queued: %v", err)
	}
	if _, err := s.Approve(ctx, uuid.New(), v.ID, nil); !ent.IsNotFound(err) {
		t.Fatalf("approve other tenant: %v", err)
	}

	// One more fits; the 6th live draft for the tenant is refused.
	if _, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "fifth"}); err != nil {
		t.Fatalf("5th draft: %v", err)
	}
	if _, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "sixth"}); !errors.Is(err, ErrDraftLimit) {
		t.Fatalf("6th draft: %v", err)
	}

	// Ready with a file each: discard expires the file now; approve clears its
	// file's expiry and supersedes the brief's other ready draft (24 h).
	exp := time.Now().Add(DraftTTL)
	for _, x := range vs[:3] {
		_ = s.db.CreativeVersion.UpdateOneID(x.ID).SetStatus(StatusReady).Exec(ctx)
		s.db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(x.ID).SetStorageKey("k/" + x.ID.String()).
			SetURL("https://media.example.com/" + x.ID.String() + ".jpg").SetExpiresAt(exp).SaveX(ctx)
	}
	dc, err := s.Discard(ctx, tid, vs[1].ID)
	if err != nil || dc.Status != StatusRejected || dc.Edges.Assets[0].ExpiresAt == nil || dc.Edges.Assets[0].ExpiresAt.After(time.Now()) {
		t.Fatalf("discard: %v %+v", err, dc)
	}
	user := uuid.New()
	ap, err := s.Approve(ctx, tid, vs[0].ID, &user)
	if err != nil || ap.Status != StatusApproved || ap.ApprovedBy == nil || *ap.ApprovedBy != user {
		t.Fatalf("approve: %v %+v", err, ap)
	}
	if ap.Edges.Assets[0].ExpiresAt != nil {
		t.Fatal("approved file must not expire")
	}
	if _, err := s.Approve(ctx, tid, vs[0].ID, &user); !errors.Is(err, ErrConflict) {
		t.Fatalf("second approve: %v", err)
	}
	sup, _ := s.GetVersion(ctx, tid, vs[2].ID)
	if sup.Status != StatusSuperseded || sup.Edges.Assets[0].ExpiresAt.After(time.Now().Add(supersededTTL+time.Minute)) {
		t.Fatalf("sibling: %s %v", sup.Status, sup.Edges.Assets[0].ExpiresAt)
	}
	if rj, _ := s.GetVersion(ctx, tid, vs[1].ID); rj.Status != StatusRejected {
		t.Fatalf("a rejected draft must stay rejected: %s", rj.Status)
	}

	full, err := s.GetBrief(ctx, tid, b.ID)
	if err != nil || len(full.Edges.Versions) != 5 || full.Edges.Versions[0].VersionNo != 5 {
		t.Fatalf("get brief: %v", err)
	}
}

// fakeAI answers like marketflow-ai's /api/v1/internal/creative/poster and keeps
// what it was sent, decoded with the field names the AI handler reads.
type fakeAI struct {
	mu     sync.Mutex
	got    []map[string]any
	status int
}

func (f *fakeAI) server(t *testing.T, key string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/internal/creative/poster" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if !httpware.ValidServiceKey(r, key) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.got = append(f.got, body)
		status := f.status
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"not enough AI credits for this poster"}`))
			return
		}
		vid := body["version_id"].(string)
		if scene, _ := body["scene_url"].(string); scene != "" {
			if strings.Contains(scene, "gone") {
				w.WriteHeader(http.StatusGone)
				_, _ = w.Write([]byte(`{"error":"the original image is no longer stored; make a new poster"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"poster": map[string]any{
					"url": "https://media.example.com/" + vid + ".jpg", "key": "marketflow/t/creative-drafts/posters/" + vid + ".jpg",
					"width": 1080, "height": 1350, "bytes": 99, "route": "recompose", "scene_url": scene, "attempts": 1,
				},
				"credits_charged": 0,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"poster": map[string]any{
				"url": "https://media.example.com/" + vid + ".jpg", "key": "marketflow/t/creative-drafts/posters/" + vid + ".jpg",
				"width": 1080, "height": 1350, "bytes": 123456, "sha256": "abc", "route": "fal/seedream-v5-lite",
				"cost_usd": 0.035, "source_url": "https://fal.media/scene.png", "attempts": 1,
				"scene_key": "marketflow/t/creative-drafts/posters/" + vid + "-scene.jpg",
				"scene_url": "https://media.example.com/" + vid + "-scene.jpg",
			},
			"credits_charged": 2,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRendererMakesQueuedPosters(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	v, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "jar on linen", Headline: "Soft skin", Price: "KES 850", CTA: "Order on WhatsApp", Tier: "premium"})
	if err != nil {
		t.Fatal(err)
	}

	ai := &fakeAI{}
	srv := ai.server(t, "svc-key")
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop())
	r.Tick(ctx)

	got, err := s.GetVersion(ctx, tid, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The poster and its clean scene (kept for free text edits).
	if got.Status != StatusReady || got.Credits != 2 || len(got.Edges.Assets) != 2 {
		t.Fatalf("after render: status %s credits %d assets %d err %q", got.Status, got.Credits, len(got.Edges.Assets), got.Error)
	}
	var a *ent.CreativeAsset
	for _, x := range got.Edges.Assets {
		if x.Kind == AssetImage {
			a = x
		} else if x.Kind != AssetScene || x.ExpiresAt == nil {
			t.Fatalf("scene asset: %+v", x)
		}
	}
	if a.Width != 1080 || a.ExpiresAt == nil || !strings.HasSuffix(a.StorageKey, v.ID.String()+".jpg") {
		t.Fatalf("asset: %+v", a)
	}
	if got.Metadata["scene_url"] != "https://fal.media/scene.png" || got.Metadata["route"] != "fal/seedream-v5-lite" {
		t.Fatalf("metadata: %v", got.Metadata)
	}

	if len(ai.got) != 1 {
		t.Fatalf("ai calls: %d", len(ai.got))
	}
	call := ai.got[0]
	want := map[string]any{
		"tenant_id": tid.String(), "version_id": v.ID.String(), "prompt": "jar on linen", "format": "1080x1350",
		"tier": "premium", "headline": "Soft skin", "price": "KES 850", "cta": "Order on WhatsApp",
		"accent": "#1f7a4d", "logo_url": "https://example.com/logo.png", "charge": true,
	}
	for k, w := range want {
		if call[k] != w {
			t.Errorf("ai request %s = %v, want %v", k, call[k], w)
		}
	}
	if photos, _ := call["photo_urls"].([]any); len(photos) != 1 || photos[0] != "https://example.com/shea.jpg" {
		t.Errorf("photo_urls fall back to the offering's: %v", call["photo_urls"])
	}
	if pal, _ := call["palette"].([]any); len(pal) != 2 {
		t.Errorf("palette: %v", call["palette"])
	}

	// Nothing left queued: a second tick calls nobody.
	r.Tick(ctx)
	if len(ai.got) != 1 {
		t.Fatalf("a ready version was rendered again")
	}

	// A text-only revision: same scene, new words, never charged.
	rev, err := s.Revise(ctx, tid, v.ID, ReviseInput{Price: ptr("KES 750")})
	if err != nil || rev.ParentVersionID == nil || *rev.ParentVersionID != v.ID || rev.Copy["price"] != "KES 750" || rev.Copy["headline"] != "Soft skin" {
		t.Fatalf("revise: %v %+v", err, rev)
	}
	r.Tick(ctx)
	if len(ai.got) != 2 || ai.got[1]["charge"] != false || !strings.HasSuffix(ai.got[1]["scene_url"].(string), v.ID.String()+"-scene.jpg") {
		t.Fatalf("revision request: %v", ai.got[len(ai.got)-1])
	}
	if done, _ := s.GetVersion(ctx, tid, rev.ID); done.Status != StatusReady || done.Credits != 0 {
		t.Fatalf("revision: %s credits %d", done.Status, done.Credits)
	}
	// Revising the revision reuses the same scene.
	rev2, err := s.Revise(ctx, tid, rev.ID, ReviseInput{CTA: ptr("Call now")})
	if err != nil || rev2.Metadata["scene_url"] != ai.got[1]["scene_url"] {
		t.Fatalf("second revision: %v %v", err, rev2)
	}
	// A purged scene fails the revision with a clear message.
	_ = db.CreativeVersion.UpdateOneID(rev2.ID).SetMetadata(map[string]any{"recompose": true, "scene_url": "https://media.example.com/gone.jpg"}).Exec(ctx)
	r.Tick(ctx)
	if gone, _ := s.GetVersion(ctx, tid, rev2.ID); gone.Status != StatusFailed || gone.Error != errSceneGone.Error() {
		t.Fatalf("gone scene: %s %q", gone.Status, gone.Error)
	}
	// A queued version cannot be revised.
	q, _ := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "x"})
	if _, err := s.Revise(ctx, tid, q.ID, ReviseInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("revise queued: %v", err)
	}
}

func TestExtendOnce(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	v, _ := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "a"})
	if _, err := s.Extend(ctx, tid, v.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a queued draft cannot be extended: %v", err)
	}
	_ = db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusReady).Exec(ctx)
	db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetStorageKey("k").SetURL("https://x/p.jpg").
		SetExpiresAt(time.Now().Add(DraftTTL)).SaveX(ctx)
	ext, err := s.Extend(ctx, tid, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exp := ext.Edges.Assets[0].ExpiresAt; exp == nil || exp.Before(v.CreatedAt.Add(extendedTTL-time.Minute)) {
		t.Fatalf("extended expiry: %v", exp)
	}
	if _, err := s.Extend(ctx, tid, v.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("second extension: %v", err)
	}
}

func TestRendererRecordsNoCredits(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	v, _ := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "jar"})

	ai := &fakeAI{status: http.StatusPaymentRequired}
	r := NewRenderer(db, ai.server(t, "k").URL, "k", zap.NewNop())
	r.Tick(ctx)
	got, _ := s.GetVersion(ctx, tid, v.ID)
	if got.Status != StatusFailed || got.Error != errNoCredits.Error() {
		t.Fatalf("status %s error %q", got.Status, got.Error)
	}
}

func TestRendererRecoversStale(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	v1, _ := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "a"})
	v2, _ := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "b"})
	_ = db.CreativeVersion.UpdateOneID(v1.ID).SetStatus(StatusRendering).SetMetadata(map[string]any{"attempts": 1}).Exec(ctx)
	_ = db.CreativeVersion.UpdateOneID(v2.ID).SetStatus(StatusRendering).SetMetadata(map[string]any{"attempts": maxAttempts}).Exec(ctx)

	r := NewRenderer(db, "http://127.0.0.1:1", "k", zap.NewNop())
	r.nowFn = func() time.Time { return time.Now().Add(staleAfter + time.Minute) }
	r.recoverStale(ctx)

	g1, _ := s.GetVersion(ctx, tid, v1.ID)
	g2, _ := s.GetVersion(ctx, tid, v2.ID)
	if g1.Status != StatusQueued || g2.Status != StatusFailed {
		t.Fatalf("stale recovery: %s %s", g1.Status, g2.Status)
	}
}

func TestReviewLinks(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	v, _ := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "a"})
	rv := NewReviews(db, []byte("0123456789abcdef0123456789abcdef"))

	if _, _, err := rv.Link(ctx, tid, v.ID, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("a queued draft cannot be shared: %v", err)
	}
	_ = db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusReady).Exec(ctx)
	db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetStorageKey("k").SetURL("https://media.example.com/p.jpg").SaveX(ctx)
	if _, _, err := rv.Link(ctx, uuid.New(), v.ID, 0); !ent.IsNotFound(err) {
		t.Fatalf("other tenant: %v", err)
	}

	token, exp, err := rv.Link(ctx, tid, v.ID, 0)
	if err != nil || exp.Before(time.Now().Add(71*time.Hour)) || exp.After(time.Now().Add(73*time.Hour)) {
		t.Fatalf("link: %v %v", err, exp)
	}
	if url, err := rv.Open(ctx, token); err != nil || url != "https://media.example.com/p.jpg" {
		t.Fatalf("open: %q %v", url, err)
	}

	// Through the handler: 302, noindex.
	h := NewHandler(s, rv, zap.NewNop())
	req := httptest.NewRequest(http.MethodGet, "/r/"+token, nil)
	req = withURLParam(req, "token", token)
	rec := httptest.NewRecorder()
	h.OpenReview(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://media.example.com/p.jpg" || !strings.Contains(rec.Header().Get("X-Robots-Tag"), "noindex") {
		t.Fatalf("handler: %d %v", rec.Code, rec.Header())
	}

	// Tampered, wrong key, expired.
	raw := []byte(token)
	raw[5] ^= 1
	if _, err := rv.Open(ctx, string(raw)); err == nil {
		t.Fatal("tampered token opened")
	}
	if _, err := NewReviews(db, []byte("another key another key another ")).Open(ctx, token); err == nil {
		t.Fatal("token opened with another key")
	}
	late := NewReviews(db, rv.key)
	late.nowFn = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if _, err := late.Open(ctx, token); err == nil {
		t.Fatal("expired token opened")
	}

	// Revoking ends the old link; a new one works.
	if err := rv.Revoke(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rv.Open(ctx, token); err == nil {
		t.Fatal("revoked token opened")
	}
	rec = httptest.NewRecorder()
	h.OpenReview(rec, withURLParam(httptest.NewRequest(http.MethodGet, "/r/"+token, nil), "token", token))
	if rec.Code != http.StatusGone {
		t.Fatalf("revoked link through the handler: %d", rec.Code)
	}
	fresh, _, _ := rv.Link(ctx, tid, v.ID, 0)
	if _, err := rv.Open(ctx, fresh); err != nil {
		t.Fatalf("fresh link after revoke: %v", err)
	}

	// A discarded draft is no longer shown.
	if _, err := s.Discard(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rv.Open(ctx, fresh); err == nil {
		t.Fatal("discarded draft still shown")
	}
}

func TestPurgerDeletesExpiredFiles(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	v, _ := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "a"})

	root := t.TempDir()
	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: root, LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	put := func(name string, expires time.Time) *ent.CreativeAsset {
		key, err := store.Key(objectstore.Owner{TenantID: tid.String()}, TypeCreativeDrafts, "posters", name+".jpg")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, jpegBytes, "image/jpeg"); err != nil {
			t.Fatal(err)
		}
		return db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetStorageKey(key).
			SetURL(store.URL(key)).SetExpiresAt(expires).SaveX(ctx)
	}
	old := put("old", time.Now().Add(-time.Hour))
	keep := put("keep", time.Now().Add(time.Hour))

	p := NewPurger(db, store, zap.NewNop())
	n, err := p.Purge(ctx)
	if err != nil || n < 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if got := db.CreativeAsset.GetX(ctx, old.ID); got.PurgedAt == nil {
		t.Fatal("expired file not marked purged")
	}
	if got := db.CreativeAsset.GetX(ctx, keep.ID); got.PurgedAt != nil {
		t.Fatal("live file purged")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(old.StorageKey))); !os.IsNotExist(err) {
		t.Fatalf("expired file still on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(keep.StorageKey))); err != nil {
		t.Fatalf("live file gone: %v", err)
	}
}

// jpegBytes is a minimal valid JPEG (1x1), enough for the store's type sniffing.
var jpegBytes = func() []byte {
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil)
	return buf.Bytes()
}()

func withURLParam(r *http.Request, key, value string) *http.Request {
	rc := chi.NewRouteContext()
	rc.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
}
