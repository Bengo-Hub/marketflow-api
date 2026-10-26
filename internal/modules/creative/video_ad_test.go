package creative

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"
)

const scriptReply = `{"script":{"format":"product in motion","hooks":["Dry skin?","",""],"scenes":[
 {"id":"s1","duration_s":5,"shot":"product_clip","asset":1,"visual":"jar","voiceover":"Dry skin in the morning? Try this.","caption":"Dry skin?"},
 {"id":"s2","duration_s":5,"shot":"end_card","visual":"","voiceover":"Shea butter from Kisumu. Order today.","caption":"Order today"}],"cta":"Order"},
 "findings":[{"quote":"script","issue":"AI tell","severity":"low","fix":""}],"credits_charged":1,"cost_usd":0.002}`

// A video ad is a script first (reviewed, editable, 1 credit), then a paid
// render whose result marketflow-ai pushes back.
func TestVideoAdScriptEditRender(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)

	for name, in := range map[string]VideoAdInput{
		"seconds":  {Seconds: 90},
		"delivery": {Delivery: "opera"},
		"format":   {Format: "800x600"},
		"gender":   {VoiceGender: "robot"},
		"photo":    {PhotoURLs: []string{"file:///etc/passwd"}},
	} {
		if _, err := s.CreateVideoAd(ctx, tid, b.ID, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Delivery: "ugc", VoiceAccent: "kenyan", CTA: "Order on WhatsApp"})
	if err != nil {
		t.Fatal(err)
	}
	card, _ := v.Copy["end_card"].(map[string]any)
	if v.Kind != KindVideoAd || v.Status != StatusQueued || v.Format != "1080x1920" || intOf(v.Metadata["seconds"]) != 30 ||
		stage(v) != StageScript || len(strs(v.Metadata["photo_urls"])) != 1 || card["headline"] != "Shea butter 250g" || card["cta"] != "Order on WhatsApp" {
		t.Fatalf("video ad = %+v", v)
	}

	ai := &fakeJobs{jobs: map[string]string{}, scriptBody: scriptReply}
	srv := ai.server(t, "svc-key")
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop())
	r.Tick(ctx)
	if len(ai.scripts) != 1 {
		t.Fatalf("script calls: %d", len(ai.scripts))
	}
	sc := ai.scripts[0]
	if sc["offering"] != "Shea butter 250g" || sc["photos"] != float64(1) || sc["seconds"] != float64(30) || sc["language"] != "en" || sc["charge"] != true {
		t.Fatalf("script call: %v", sc)
	}
	got, _ := s.GetVersion(ctx, tid, v.ID)
	if got.Status != StatusScriptReady || got.Credits != 1 || got.Copy["script"] == nil || got.Metadata["findings"] == nil {
		t.Fatalf("after script: %s credits %d %v %q", got.Status, got.Credits, got.Copy, got.Error)
	}

	// A script waits for the owner: it cannot be approved, and bad edits are refused.
	if _, err := s.Approve(ctx, tid, v.ID, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve a script: %v", err)
	}
	scenes := func(shot string, asset int) *VideoScript {
		return &VideoScript{Scenes: []ScriptScene{
			{Shot: shot, Asset: asset, Voiceover: "Soft skin all week."},
			{Shot: "end_card", Voiceover: "Order on WhatsApp."}}}
	}
	for name, bad := range map[string]*VideoScript{
		"presenter":    scenes("presenter", 0),
		"photo 3 of 1": scenes("product_clip", 3),
		"no footage":   scenes("owner_footage", 0),
		"one scene":    {Scenes: []ScriptScene{{Shot: "end_card", Voiceover: "Hi"}}},
		"no voice":     {Scenes: []ScriptScene{{Shot: "b_roll"}, {Shot: "end_card"}}},
	} {
		if _, err := s.UpdateScript(ctx, tid, v.ID, ScriptInput{Script: bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	edited, err := s.UpdateScript(ctx, tid, v.ID, ScriptInput{Script: scenes("product_clip", 1),
		EndCard: map[string]any{"headline": "Shea butter", "price": "KES 850", "cta": "Order on WhatsApp"}})
	if err != nil {
		t.Fatal(err)
	}
	if ec, _ := edited.Copy["end_card"].(map[string]any); ec["price"] != "KES 850" || edited.Metadata["script_edited_at"] == nil {
		t.Fatalf("edit not kept: %v", edited.Copy)
	}

	// The render: queued, then started with the reviewed script and the voice choice.
	if _, err := s.RenderVideoAd(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	if len(ai.started) != 1 || ai.paths[0] != "/api/v1/internal/creative/ad" {
		t.Fatalf("ad starts: %v", ai.paths)
	}
	ad := ai.started[0]
	sentScenes, _ := ad["scenes"].([]any)
	if len(sentScenes) != 2 || ad["delivery"] != "ugc" || ad["voice_accent"] != "kenyan" || ad["price"] != "KES 850" ||
		ad["accent"] != "#1f7a4d" || ad["format"] != "1080x1920" || len(ad["photo_urls"].([]any)) != 1 {
		t.Fatalf("ad call: %v", ad)
	}
	if _, err := s.UpdateScript(ctx, tid, v.ID, ScriptInput{Script: scenes("b_roll", 0)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a script being rendered cannot change: %v", err)
	}
	h := NewHandler(s, nil, nil, zap.NewNop()).WithJobs(NewRenderer(db, "", "", zap.NewNop()))
	done := `{"status":"done","credits_charged":12,"result":{"url":"https://media.example.com/ad.mp4","key":"marketflow/t/video-drafts/ads/a.mp4",
		"bytes":4096000,"seconds":23.5,"width":1080,"height":1920,"sha256":"adhash","route":"elevenlabs/eleven-v4-turbo","cost_usd":0.02,"caption_lines":9}}`
	if code := callback(t, h, v.ID, done); code != http.StatusNoContent {
		t.Fatalf("callback: %d", code)
	}
	got, _ = s.GetVersion(ctx, tid, v.ID)
	if got.Status != StatusReady || got.Credits != 13 || len(got.Edges.Assets) != 1 || got.Edges.Assets[0].DurationMs != 23500 ||
		got.Edges.Assets[0].Kind != AssetVideo || intOf(got.Metadata["caption_lines"]) != 9 {
		t.Fatalf("after render: %s credits %d assets %+v", got.Status, got.Credits, got.Edges.Assets)
	}
}

// A blocked script keeps the review's findings for the owner; a render
// marketflow-ai refuses shows its reason; a script can be discarded.
func TestVideoAdFailures(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	ai := &fakeJobs{jobs: map[string]string{}, scriptCode: http.StatusUnprocessableEntity,
		scriptBody: `{"error":"the script did not pass the review; nothing was charged","findings":[{"issue":"compliance: medical claim","severity":"high"}]}`}
	srv := ai.server(t, "svc-key")
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop())

	blocked, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{})
	if err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	got, _ := s.GetVersion(ctx, tid, blocked.ID)
	if got.Status != StatusFailed || !strings.Contains(got.Error, "did not pass the review") || got.Metadata["findings"] == nil {
		t.Fatalf("blocked script: %s %q %v", got.Status, got.Error, got.Metadata["findings"])
	}

	ai.scriptCode, ai.scriptBody = 0, scriptReply
	ai.startStatus, ai.startBody = http.StatusBadRequest, `{"error":"scene 1 needs an AI presenter, which this video type does not include"}`
	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{})
	if err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx) // script
	if _, err := s.RenderVideoAd(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx) // render refused by marketflow-ai
	got, _ = s.GetVersion(ctx, tid, v.ID)
	if got.Status != StatusFailed || !strings.Contains(got.Error, "AI presenter") {
		t.Fatalf("refused render: %s %q", got.Status, got.Error)
	}

	third, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{})
	if err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	if d, err := s.Discard(ctx, tid, third.ID); err != nil || d.Status != StatusRejected {
		t.Fatalf("discard a script: %v %+v", err, d)
	}
}
