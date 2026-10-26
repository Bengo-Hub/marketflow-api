package creative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
)

// fakeJobs answers like marketflow-ai's media job routes: POST /clip or /ad
// starts a job (202 running, or the status set in startStatus), GET
// /jobs/{id} reports the state set in jobs, and POST /script answers with
// scriptStatus and scriptBody.
type fakeJobs struct {
	mu          sync.Mutex
	started     []map[string]any
	paths       []string
	jobs        map[string]string // version id -> GET reply body; missing is a 404
	startStatus int
	startBody   string
	scripts     []map[string]any
	scriptCode  int
	scriptBody  string
}

func (f *fakeJobs) server(t *testing.T, key string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !httpware.ValidServiceKey(r, key) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == "/api/v1/internal/creative/clip" || r.URL.Path == "/api/v1/internal/creative/ad"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.started, f.paths = append(f.started, body), append(f.paths, r.URL.Path)
			if f.startStatus != 0 {
				w.WriteHeader(f.startStatus)
				_, _ = w.Write([]byte(f.startBody))
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"running","credits_charged":14,"attempt":1}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/internal/creative/script":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.scripts = append(f.scripts, body)
			code := f.scriptCode
			if code == 0 {
				code = http.StatusOK
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(f.scriptBody))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/internal/creative/jobs/"):
			body, ok := f.jobs[strings.TrimPrefix(r.URL.Path, "/api/v1/internal/creative/jobs/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no video job for this version"}`))
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// finishedPoster makes a ready poster with its scene, the source of a motion poster.
func finishedPoster(t *testing.T, s *Service, tid uuid.UUID, b *ent.CreativeBrief) *ent.CreativeVersion {
	t.Helper()
	ctx := context.Background()
	v, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "jar on linen", Headline: "Soft skin", Price: "KES 850", CTA: "Order now", Format: "1080x1920"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusReady).Exec(ctx)
	exp := time.Now().Add(DraftTTL)
	s.db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind(AssetImage).SetStorageKey("p/" + v.ID.String()).
		SetURL("https://media.example.com/" + v.ID.String() + ".jpg").SetSha256("posterhash").SetExpiresAt(exp).SaveX(ctx)
	s.db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind(AssetScene).SetStorageKey("s/" + v.ID.String()).
		SetURL("https://media.example.com/" + v.ID.String() + "-scene.jpg").SetExpiresAt(exp).SaveX(ctx)
	return v
}

func callback(t *testing.T, h *Handler, id uuid.UUID, body string) int {
	t.Helper()
	req := withURLParam(httptest.NewRequest(http.MethodPost, "/api/v1/internal/creative/jobs/"+id.String(), bytes.NewBufferString(body)), "id", id.String())
	rec := httptest.NewRecorder()
	h.JobCallback(rec, req)
	return rec.Code
}

func TestMotionPosterThroughCallback(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)

	queued, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "not made yet"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMotion(ctx, tid, queued.ID, MotionInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a queued poster cannot be animated: %v", err)
	}
	src := finishedPoster(t, s, tid, b)
	if _, err := s.CreateMotion(ctx, tid, src.ID, MotionInput{Seconds: 30}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("30 seconds: %v", err)
	}
	if _, err := s.CreateMotion(ctx, uuid.New(), src.ID, MotionInput{}); !ent.IsNotFound(err) {
		t.Fatalf("another tenant: %v", err)
	}
	mv, err := s.CreateMotion(ctx, tid, src.ID, MotionInput{Prompt: "slow push in"})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Kind != KindVideo || mv.ParentVersionID == nil || *mv.ParentVersionID != src.ID || mv.Format != "1080x1920" ||
		intOf(mv.Metadata["seconds"]) != 5 || !strings.HasSuffix(str(mv.Metadata["scene_url"]), "-scene.jpg") {
		t.Fatalf("motion version: %+v", mv)
	}
	if _, err := s.CreateMotion(ctx, tid, mv.ID, MotionInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a video cannot be animated: %v", err)
	}
	if _, err := s.Revise(ctx, tid, mv.ID, ReviseInput{Headline: ptr("x")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a video cannot be revised: %v", err)
	}
	_ = db.CreativeVersion.UpdateOneID(queued.ID).SetStatus(StatusRejected).Exec(ctx) // keep it out of the poster renderer

	clips := &fakeJobs{jobs: map[string]string{}}
	srv := clips.server(t, "svc-key")
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop())
	r.Tick(ctx)
	if len(clips.started) != 1 {
		t.Fatalf("clip starts: %d", len(clips.started))
	}
	sent := clips.started[0]
	if sent["version_id"] != mv.ID.String() || sent["seconds"] != float64(5) || sent["prompt"] != "slow push in" ||
		sent["headline"] != "Soft skin" || sent["accent"] != "#1f7a4d" || sent["charge"] != true || sent["format"] != "1080x1920" {
		t.Fatalf("clip call: %v", sent)
	}
	got, _ := s.GetVersion(ctx, tid, mv.ID)
	if got.Status != StatusRendering {
		t.Fatalf("after start: %s %q", got.Status, got.Error)
	}
	// Recent news: the fallback poll leaves it alone.
	r.Tick(ctx)
	if len(clips.started) != 1 {
		t.Fatal("a running clip must not be started again")
	}

	h := NewHandler(s, nil, nil, zap.NewNop()).WithJobs(NewRenderer(db, "", "", zap.NewNop()))
	done := `{"status":"done","credits_charged":14,"result":{"url":"https://media.example.com/c.mp4","key":"marketflow/t/clips/c.mp4",
		"bytes":2048000,"seconds":5,"width":1080,"height":1920,"sha256":"cliphash","route":"fal/seedance","cost_usd":0.3,"attempts":1}}`
	if code := callback(t, h, mv.ID, done); code != http.StatusNoContent {
		t.Fatalf("callback: %d", code)
	}
	if code := callback(t, h, mv.ID, done); code != http.StatusNoContent {
		t.Fatalf("repeated callback: %d", code)
	}
	if code := callback(t, h, uuid.New(), done); code != http.StatusNotFound {
		t.Fatalf("unknown version: %d", code)
	}
	got, _ = s.GetVersion(ctx, tid, mv.ID)
	if got.Status != StatusReady || got.Credits != 14 || len(got.Edges.Assets) != 1 {
		t.Fatalf("after callback: %s credits %d assets %d", got.Status, got.Credits, len(got.Edges.Assets))
	}
	a := got.Edges.Assets[0]
	if a.Kind != AssetVideo || a.DurationMs != 5000 || a.ExpiresAt == nil || a.Sha256 != "cliphash" {
		t.Fatalf("clip asset: %+v", a)
	}

	// Approving the clip keeps its evidence and leaves the poster draft alone.
	ap, err := s.Approve(ctx, tid, mv.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ev, _ := ap.Metadata["approval"].(map[string]any); ev["sha256"] != "cliphash" {
		t.Fatalf("approval evidence: %v", ap.Metadata["approval"])
	}
	if p, _ := s.GetVersion(ctx, tid, src.ID); p.Status != StatusReady {
		t.Fatalf("the poster must stay in review: %s", p.Status)
	}
}

func TestMotionPosterFallbackPoll(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	src := finishedPoster(t, s, tid, b)

	clips := &fakeJobs{jobs: map[string]string{}}
	srv := clips.server(t, "svc-key")
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop())
	quiet := time.Now().Add(-jobPollAfter - time.Minute)
	rendering := func() *ent.CreativeVersion {
		v, err := s.CreateMotion(ctx, tid, src.ID, MotionInput{})
		if err != nil {
			t.Fatal(err)
		}
		meta := copyMap(v.Metadata)
		meta["attempts"], meta["job_started_at"] = 1, time.Now().UTC().Format(time.RFC3339)
		db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusRendering).SetMetadata(meta).SetUpdatedAt(quiet).ExecX(ctx)
		return v
	}
	refunded, lost, running := rendering(), rendering(), rendering()
	clips.jobs[refunded.ID.String()] = `{"status":"failed","refunded":true,"error":"the video could not be made right now; nothing was charged"}`
	clips.jobs[running.ID.String()] = `{"status":"running","credits_charged":14}`
	// lost has no job at marketflow-ai: back in the queue.

	r.pollJobs(ctx)

	if v, _ := s.GetVersion(ctx, tid, refunded.ID); v.Status != StatusFailed || !strings.Contains(v.Error, "nothing was charged") {
		t.Fatalf("refunded: %s %q", v.Status, v.Error)
	}
	if v, _ := s.GetVersion(ctx, tid, lost.ID); v.Status != StatusQueued {
		t.Fatalf("lost: %s", v.Status)
	}
	v, _ := s.GetVersion(ctx, tid, running.ID)
	if v.Status != StatusRendering || v.UpdatedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("running: %s, polled %v", v.Status, v.UpdatedAt)
	}
	if n := db.CreativeAsset.Query().Where(creativeasset.TenantID(tid), creativeasset.KindEQ(AssetVideo)).CountX(ctx); n != 0 {
		t.Fatalf("no clip was made: %d", n)
	}
}
