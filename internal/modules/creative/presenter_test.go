package creative

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"
)

const presenterScript = `{"script":{"format":"presenter","hooks":["Holiday screen time?","",""],"scenes":[
 {"id":"s1","duration_s":4,"shot":"presenter","visual":"mother at home","voiceover":"Parents, is the holiday all screen time?","caption":"Holiday screen time?"},
 {"id":"s2","duration_s":4,"shot":"b_roll","visual":"kids coding at laptops","voiceover":"Kids build their own games.","caption":""},
 {"id":"s3","duration_s":4,"shot":"end_card","visual":"","voiceover":"WhatsApp us today.","caption":"WhatsApp us"}],"cta":"WhatsApp us"},
 "findings":[],"credits_charged":1,"cost_usd":0.002}`

// A presenter ad: the script, then the storyboard (actor kept on the brief), its approval,
// the render with the actor, stills and no cache, then a scene revision that sends the
// first render's scene cache. A storyboard lost past its attempts is refunded.
func TestPresenterAdStoryboardRenderRevision(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)

	if _, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Kind: "cinema"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kind: %v", err)
	}
	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Kind: VideoPresenter, ActorPrompt: "a warm Kenyan mother", Delivery: "kids"})
	if err != nil {
		t.Fatal(err)
	}
	ai := &fakeJobs{jobs: map[string]string{}, scriptBody: presenterScript}
	srv := ai.server(t, "svc-key")
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop())
	r.Tick(ctx)
	if ai.scripts[0]["kind"] != VideoPresenter {
		t.Fatalf("the script writer must know it may use a presenter: %v", ai.scripts[0])
	}
	if _, err := s.RenderVideoAd(ctx, tid, v.ID); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "storyboard") {
		t.Fatalf("a presenter render needs the approved storyboard: %v", err)
	}

	// Storyboard: started with the scenes and the actor prompt, settled by the push.
	if _, err := s.MakeStoryboard(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	if n := len(ai.paths); n != 1 || ai.paths[0] != "/api/v1/internal/creative/storyboard" || ai.started[0]["actor_prompt"] != "a warm Kenyan mother" {
		t.Fatalf("storyboard start: %v %v", ai.paths, ai.started)
	}
	h := NewHandler(s, nil, nil, zap.NewNop()).WithJobs(r)
	// A render report cannot settle a storyboard.
	if code := callback(t, h, v.ID, `{"kind":"render","status":"done","result":{"url":"https://x/a.mp4","key":"k"}}`); code != http.StatusNoContent {
		t.Fatalf("callback: %d", code)
	}
	if got, _ := s.GetVersion(ctx, tid, v.ID); got.Status != StatusRendering {
		t.Fatalf("a render report settled the storyboard: %s", got.Status)
	}
	board := `{"kind":"storyboard","status":"done","credits_charged":1,"result":{"url":"","key":"","storyboard":{
		"actor":{"url":"https://media.example/actor.jpg","key":"marketflow/t/creative-drafts/posters/a-actor.jpg","prompt":"a warm Kenyan mother","seed":42},
		"stills":[{"scene_id":"s1","url":"https://media.example/actor.jpg","source":"actor"},
		{"scene_id":"s2","url":"https://media.example/s2.jpg","key":"marketflow/t/creative-drafts/posters/a-sb-s2.jpg","source":"generated"},
		{"scene_id":"s3","url":"https://media.example/p.jpg","source":"photo"}],"cost_usd":0.007}}}`
	if code := callback(t, h, v.ID, board); code != http.StatusNoContent {
		t.Fatalf("storyboard callback: %d", code)
	}
	got, _ := s.GetVersion(ctx, tid, v.ID)
	if got.Status != StatusScriptReady || got.Credits != 2 || stage(got) != StageScript || len(got.Edges.Assets) != 2 {
		t.Fatalf("after storyboard: %s credits %d stage %s assets %d", got.Status, got.Credits, stage(got), len(got.Edges.Assets))
	}
	brief, _ := s.GetBrief(ctx, tid, b.ID)
	if actor, _ := brief.Metadata["actor"].(map[string]any); actor["url"] != "https://media.example/actor.jpg" {
		t.Fatalf("the actor is kept on the brief: %v", brief.Metadata["actor"])
	}

	// Quote, approve, render.
	data, status, err := r.QuoteVideoAd(ctx, got)
	if err != nil || status != http.StatusOK || !strings.Contains(string(data), "credits_needed") {
		t.Fatalf("quote: %d %s %v", status, data, err)
	}
	if ai.started[len(ai.started)-1]["charge"] != false {
		t.Fatal("a quote must never charge")
	}
	if _, err := s.ApproveStoryboard(ctx, tid, v.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenderVideoAd(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	ad := ai.started[len(ai.started)-1]
	stills, _ := ad["stills"].(map[string]any)
	if ad["kind"] != VideoPresenter || ad["actor_url"] != "https://media.example/actor.jpg" || ad["seed"] != float64(42) ||
		stills["s2"] != "https://media.example/s2.jpg" || len(stills) != 1 || ad["scene_cache"] != nil {
		t.Fatalf("render call: %v", ad)
	}
	done := `{"kind":"render","status":"done","credits_charged":40,"result":{"url":"https://media.example/ad.mp4","key":"marketflow/t/video-drafts/ads/a.mp4",
		"bytes":100,"seconds":20,"width":1080,"height":1920,"sha256":"h","route":"higgsfield/seedance-2.5-480p","voice":"Yna Agalo - Kenyan Lady",
		"scenes":[{"hash":"aa","scene_id":"s1","audio_key":"marketflow/t/audio-drafts/voice/a-scene-s1.mp3","audio_url":"https://m/a1.mp3","video_key":"marketflow/t/video-drafts/clips/a-scene-s1.mp4","video_url":"https://m/c1.mp4"},
		{"hash":"bb","scene_id":"s2","audio_key":"marketflow/t/audio-drafts/voice/a-scene-s2.mp3","audio_url":"https://m/a2.mp3"}]}}`
	if code := callback(t, h, v.ID, done); code != http.StatusNoContent {
		t.Fatalf("render callback: %d", code)
	}
	got, _ = s.GetVersion(ctx, tid, v.ID)
	if got.Status != StatusReady || got.Metadata["voice_used"] != "Yna Agalo - Kenyan Lady" || len(got.Metadata["scene_cache"].([]any)) != 2 {
		t.Fatalf("after render: %s %v", got.Status, got.Metadata)
	}
	kinds := map[string]int{}
	for _, a := range got.Edges.Assets {
		kinds[a.Kind]++
	}
	if kinds[AssetVideo] != 1 || kinds[AssetAudio] != 2 || kinds[AssetSceneClip] != 1 || kinds[AssetKeyframe] != 2 {
		t.Fatalf("assets for the purge: %v", kinds)
	}

	// Change one scene: a new script with the same storyboard and the scene cache.
	rev, err := s.ReviseVideoAd(ctx, tid, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Status != StatusScriptReady || !storyboardApproved(rev) || rev.Metadata["voice"] != "Yna Agalo - Kenyan Lady" || rev.ParentVersionID == nil {
		t.Fatalf("revision: %s %v", rev.Status, rev.Metadata)
	}
	if _, err := s.RenderVideoAd(ctx, tid, rev.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	if cache, _ := ai.started[len(ai.started)-1]["scene_cache"].([]any); len(cache) != 2 {
		t.Fatalf("the revision must send the scene cache: %v", ai.started[len(ai.started)-1])
	}

	// A storyboard lost with its pod past its attempts is refunded and back to the script.
	lost, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Kind: VideoPresenter})
	if err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	if _, err := s.MakeStoryboard(ctx, tid, lost.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	lv, _ := s.GetVersion(ctx, tid, lost.ID)
	meta := copyMap(lv.Metadata)
	meta["attempts"] = maxAttempts
	_ = db.CreativeVersion.UpdateOneID(lost.ID).SetMetadata(meta).Exec(ctx)
	if code := callback(t, h, lost.ID, `{"kind":"storyboard","status":"failed","refunded":false}`); code != http.StatusNoContent {
		t.Fatalf("lost callback: %d", code)
	}
	lv, _ = s.GetVersion(ctx, tid, lost.ID)
	refunded := false
	for _, p := range ai.paths {
		if strings.HasSuffix(p, lost.ID.String()+"/refund?kind=storyboard") {
			refunded = true
		}
	}
	if lv.Status != StatusScriptReady || !refunded || str(lv.Metadata["storyboard_error"]) == "" {
		t.Fatalf("lost storyboard: %s refunded %v %v", lv.Status, refunded, lv.Metadata)
	}
}
