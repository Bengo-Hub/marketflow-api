package creative

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// turnScript is a script the Marketing Manager's team wrote: two photo scenes and the end card.
func turnScript() *VideoScript {
	return &VideoScript{Format: "demo", Hooks: []string{"Dry skin?"}, CTA: "Order", Mood: "warm", Scenes: []ScriptScene{
		{ID: "s1", DurationS: 5, Shot: "b_roll", Voiceover: "Dry skin in the morning?", Caption: "Dry skin?"},
		{ID: "s2", DurationS: 5, Shot: "product_clip", Asset: 1, Voiceover: "Shea butter from Kisumu.", Caption: "Shea butter"},
		{ID: "s3", DurationS: 3, Shot: "end_card", Voiceover: "Order today.", Caption: "Order today"}}}
}

// A video ad sent with its script starts at script_ready with that script: the renderer never
// asks marketflow-ai for a script and no script credit is recorded. The script is checked like
// an owner's edit. Without a script nothing changes.
func TestScriptedVideoAdSkipsTheScriptJob(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)

	bad := turnScript()
	bad.Scenes[1].Asset = 3 // the brief has one photo
	if _, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Script: bad}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a script using a photo the ad does not have: %v", err)
	}
	presenter := turnScript()
	presenter.Scenes[0].Shot = "presenter"
	if _, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Script: presenter}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a presenter scene in a template ad: %v", err)
	}

	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Seconds: 45, Script: turnScript()})
	if err != nil {
		t.Fatal(err)
	}
	script, _ := v.Copy["script"].(map[string]any)
	scenes, _ := script["scenes"].([]any)
	if v.Status != StatusScriptReady || v.Credits != 0 || len(scenes) != 3 || script["mood"] != "warm" ||
		v.Metadata["script_source"] != "os_turn" || intOf(v.Metadata["seconds"]) != 45 || stage(v) != StageScript {
		t.Fatalf("scripted video ad = %s credits %d %v %v", v.Status, v.Credits, v.Copy, v.Metadata)
	}

	ai := &fakeJobs{jobs: map[string]string{}, scriptBody: scriptReply}
	srv := ai.server(t, "svc-key")
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop())
	r.Tick(ctx)
	if len(ai.scripts) != 0 || len(ai.paths) != 0 {
		t.Fatalf("a written script must not be written again: scripts %d, paths %v", len(ai.scripts), ai.paths)
	}
	got, _ := s.GetVersion(ctx, tid, v.ID)
	if got.Status != StatusScriptReady || got.Credits != 0 {
		t.Fatalf("after a tick: %s credits %d", got.Status, got.Credits)
	}

	// Unchanged path: no script, the script job runs and charges as before.
	plain, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Status != StatusQueued {
		t.Fatalf("a video ad without a script waits for its script job: %s", plain.Status)
	}
	r.Tick(ctx)
	if len(ai.scripts) != 1 {
		t.Fatalf("the script job: %d calls", len(ai.scripts))
	}
}

// A service acting for a tenant records the brief as made by the requester it names; a signed-in
// user is the creator themselves.
func TestBriefCreator(t *testing.T) {
	requester := uuid.New()
	meta := map[string]any{"source": "os_turn", "requested_by": requester.String()}
	svc := httptest.NewRequest(http.MethodPost, "/", nil)
	svc = svc.WithContext(authclient.ContextWithClaims(svc.Context(), &authclient.Claims{IsService: true, ServiceName: "marketflow-ai"}))
	if got := briefCreator(svc, meta); got == nil || *got != requester {
		t.Fatalf("service caller: %v", got)
	}
	if got := briefCreator(svc, map[string]any{"requested_by": "not-a-uuid"}); got != nil {
		t.Fatalf("a bad requester is nobody: %v", got)
	}
	user := uuid.New()
	u := httptest.NewRequest(http.MethodPost, "/", nil)
	claims := &authclient.Claims{}
	claims.Subject = user.String()
	u = u.WithContext(authclient.ContextWithClaims(u.Context(), claims))
	if got := briefCreator(u, meta); got == nil || *got != user {
		t.Fatalf("a user's metadata cannot name someone else: %v", got)
	}
}

// fakeTurnNotifier records the messages a WhatsApp turn's video would send.
type fakeTurnNotifier struct {
	mu     sync.Mutex
	events []string
	links  [][]string
	errs   []string
	got    chan struct{}
}

func (f *fakeTurnNotifier) Notify(_ context.Context, n OSTurnNote) {
	f.mu.Lock()
	f.events = append(f.events, n.Event+":"+n.RunID)
	f.links = append(f.links, n.Links)
	f.errs = append(f.errs, n.Error)
	f.mu.Unlock()
	f.got <- struct{}{}
}

// none fails if a message arrives within a short wait.
func (f *fakeTurnNotifier) none(t *testing.T, why string) {
	t.Helper()
	select {
	case <-f.got:
		t.Fatal(why)
	case <-time.After(200 * time.Millisecond):
	}
}

func (f *fakeTurnNotifier) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.got:
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

// A WhatsApp turn's video tells the requester when its storyboard is ready (the stills) and when
// the ad is finished (its link), once each; a portal brief tells nobody.
func TestOSTurnMessagesOnStoryboardAndRender(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b, err := s.CreateBrief(ctx, tid, nil, BriefInput{Title: ptr("Video ad: Shea butter"),
		Offering: map[string]any{"name": "Shea butter 250g", "photo_urls": []any{"https://example.com/shea.jpg"}},
		Metadata: map[string]any{"source": "os_turn", "channel": "whatsapp", "run_id": "run-1", "requested_by": uuid.NewString()}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Script: turnScript()})
	if err != nil {
		t.Fatal(err)
	}
	ai := &fakeJobs{jobs: map[string]string{}}
	srv := ai.server(t, "svc-key")
	notes := &fakeTurnNotifier{got: make(chan struct{}, 4)}
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop()).WithOSTurn(notes)
	h := NewHandler(s, nil, nil, zap.NewNop()).WithJobs(r)

	if _, err := s.MakeStoryboard(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	board := `{"kind":"storyboard","status":"done","credits_charged":1,"result":{"url":"","key":"","storyboard":{
		"stills":[{"scene_id":"s1","url":"https://media.example/s1.jpg","key":"marketflow/t/creative-drafts/posters/a-sb-s1.jpg","source":"generated"},
		{"scene_id":"s2","url":"https://example.com/shea.jpg","source":"photo"}],"cost_usd":0.004}}}`
	if code := callback(t, h, v.ID, board); code != http.StatusNoContent {
		t.Fatalf("storyboard callback: %d", code)
	}
	notes.wait(t)
	// A repeated report settles nothing and sends nothing.
	if code := callback(t, h, v.ID, board); code != http.StatusNoContent {
		t.Fatalf("repeated callback: %d", code)
	}

	if _, err := s.ApproveStoryboard(ctx, tid, v.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenderVideoAd(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	done := `{"kind":"render","status":"done","credits_charged":14,"result":{"url":"https://media.example/ad.mp4","key":"marketflow/t/video-drafts/ads/a.mp4",
		"bytes":100,"seconds":20,"width":1080,"height":1920,"sha256":"h","route":"template"}}`
	if code := callback(t, h, v.ID, done); code != http.StatusNoContent {
		t.Fatalf("render callback: %d", code)
	}
	notes.wait(t)

	notes.mu.Lock()
	events, links := append([]string(nil), notes.events...), append([][]string(nil), notes.links...)
	notes.mu.Unlock()
	if len(events) != 2 || events[0] != "storyboard_ready:run-1" || events[1] != "render_done:run-1" {
		t.Fatalf("messages = %v", events)
	}
	if len(links[0]) != 2 || links[0][0] != "https://media.example/s1.jpg" || links[1][0] != "https://media.example/ad.mp4" {
		t.Fatalf("links = %v", links)
	}

	// A brief made in the portal sends nothing.
	portal := newBrief(t, s, tid)
	pv, err := s.CreateVideoAd(ctx, tid, portal.ID, VideoAdInput{Script: turnScript()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeStoryboard(ctx, tid, pv.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	if code := callback(t, h, pv.ID, board); code != http.StatusNoContent {
		t.Fatalf("portal storyboard callback: %d", code)
	}
	notes.none(t, "a portal brief must not send a WhatsApp message")
}

// whatsAppTurnVideo is a WhatsApp turn's scripted video ad queued for its render.
func whatsAppTurnVideo(t *testing.T, s *Service, tid uuid.UUID, runID string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	b, err := s.CreateBrief(ctx, tid, nil, BriefInput{Title: ptr("Video ad: Shea butter"),
		Offering: map[string]any{"name": "Shea butter 250g", "photo_urls": []any{"https://example.com/shea.jpg"}},
		Metadata: map[string]any{"source": "os_turn", "channel": "whatsapp", "run_id": runID}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Script: turnScript()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenderVideoAd(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	return v.ID
}

// Every terminal render failure tells the requester once: a render that could not start, a
// refunded failure, and a job lost past its attempts. A repeated report sends nothing more.
func TestOSTurnRenderFailedEveryWay(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	ai := &fakeJobs{jobs: map[string]string{}}
	srv := ai.server(t, "svc-key")
	notes := &fakeTurnNotifier{got: make(chan struct{}, 8)}
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop()).WithOSTurn(notes)
	h := NewHandler(s, nil, nil, zap.NewNop()).WithJobs(r)

	// 1. marketflow-ai refuses to start the render (not enough credits).
	ai.startStatus, ai.startBody = http.StatusPaymentRequired, `{"error":"not enough credits"}`
	refused := whatsAppTurnVideo(t, s, tid, "run-refused")
	r.Tick(ctx)
	notes.wait(t)
	if got, _ := s.GetVersion(ctx, tid, refused); got.Status != StatusFailed {
		t.Fatalf("refused render: %s", got.Status)
	}

	// 2. A refunded failure, reported twice.
	ai.startStatus, ai.startBody = 0, ""
	refunded := whatsAppTurnVideo(t, s, tid, "run-refunded")
	r.Tick(ctx)
	failed := `{"kind":"render","status":"failed","refunded":true,"error":"the presenter route is down; nothing was charged"}`
	if code := callback(t, h, refunded, failed); code != http.StatusNoContent {
		t.Fatalf("callback: %d", code)
	}
	notes.wait(t)
	callback(t, h, refunded, failed)
	notes.none(t, "a repeated failure report must not send again")

	// 3. Lost with its pod past its attempts (not refunded by marketflow-ai).
	lost := whatsAppTurnVideo(t, s, tid, "run-lost")
	r.Tick(ctx)
	v, _ := s.GetVersion(ctx, tid, lost)
	meta := copyMap(v.Metadata)
	meta["attempts"] = maxAttempts
	if err := db.CreativeVersion.UpdateOneID(lost).SetMetadata(meta).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if code := callback(t, h, lost, `{"kind":"render","status":"failed","refunded":false}`); code != http.StatusNoContent {
		t.Fatalf("lost callback: %d", code)
	}
	notes.wait(t)

	notes.mu.Lock()
	defer notes.mu.Unlock()
	want := []string{"render_failed:run-refused", "render_failed:run-refunded", "render_failed:run-lost"}
	if len(notes.events) != 3 {
		t.Fatalf("messages = %v", notes.events)
	}
	for i, e := range want {
		if notes.events[i] != e {
			t.Fatalf("messages = %v", notes.events)
		}
	}
	if notes.errs[0] != "not enough AI credits for this video" || notes.errs[1] == "" || notes.errs[2] == "" {
		t.Fatalf("reasons = %q", notes.errs)
	}
}

// marketflow-ai's background failure (media_failed) reaches the notifier; other events are refused.
func TestOSTurnEventEndpoint(t *testing.T) {
	notes := &fakeTurnNotifier{got: make(chan struct{}, 2)}
	h := NewHandler(nil, nil, nil, zap.NewNop()).WithJobs((&Renderer{}).WithOSTurn(notes))
	call := func(body string) int {
		rec := httptest.NewRecorder()
		h.OSTurnEvent(rec, httptest.NewRequest(http.MethodPost, "/api/v1/internal/creative/os-turn/events", strings.NewReader(body)))
		return rec.Code
	}
	tid := uuid.NewString()
	if code := call(`{"tenant_id":"` + tid + `","run_id":"run-1","event":"render_done","error":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("other events: %d", code)
	}
	if code := call(`{"tenant_id":"` + tid + `","run_id":"run-1","event":"media_failed","error":"Upload at least 3 brand images first"}`); code != http.StatusNoContent {
		t.Fatalf("media_failed: %d", code)
	}
	notes.wait(t)
	if notes.events[0] != "media_failed:run-1" || notes.errs[0] != "Upload at least 3 brand images first" {
		t.Fatalf("note = %v %v", notes.events, notes.errs)
	}
}
