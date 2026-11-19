package creative

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// mustApprovePlan approves a version's generation plan as it is now, as the owner does from the quote.
func mustApprovePlan(t *testing.T, s *Service, tid, id uuid.UUID) {
	t.Helper()
	v, err := s.version(context.Background(), tid, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovePlan(context.Background(), tid, id, PlanFingerprint(v), nil, nil); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
}

func TestPlanFingerprintFollowsWhatShapesThePlan(t *testing.T) {
	v := &ent.CreativeVersion{Format: "1920x1080", Tier: "standard",
		Copy: map[string]any{"script": map[string]any{"scenes": []any{map[string]any{"voiceover": "Hello"}}}},
		Metadata: map[string]any{"video_kind": VideoPresenter, "delivery": "ugc", "storyboard": map[string]any{
			"actor":  map[string]any{"url": "https://m/actor.jpg?sig=1"},
			"stills": []any{map[string]any{"scene_id": "s2", "source": "generated", "url": "https://m/s2.jpg?sig=1"}}}}}
	fp := PlanFingerprint(v)
	// A fresh signature on the same pictures is the same plan.
	v.Metadata["storyboard"].(map[string]any)["actor"] = map[string]any{"url": "https://m/actor.jpg?sig=2"}
	if PlanFingerprint(v) != fp {
		t.Fatal("a re-signed link changed the plan")
	}
	// The script, a still, the format or the delivery each make a new plan.
	for name, change := range map[string]func(*ent.CreativeVersion){
		"script":   func(v *ent.CreativeVersion) { v.Copy = map[string]any{"script": map[string]any{"scenes": []any{}}} },
		"still":    func(v *ent.CreativeVersion) { v.Metadata["storyboard"] = map[string]any{"stills": []any{}} },
		"format":   func(v *ent.CreativeVersion) { v.Format = "1080x1920" },
		"delivery": func(v *ent.CreativeVersion) { v.Metadata["delivery"] = "ad" },
	} {
		c := *v
		c.Copy, c.Metadata = copyMap(v.Copy), copyMap(v.Metadata)
		change(&c)
		if PlanFingerprint(&c) == fp {
			t.Errorf("%s did not change the plan", name)
		}
	}
	// The render gate: refused before approval, and again once the plan changed.
	if err := checkPlanForRender(v); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "approve the generation plan") {
		t.Fatalf("unapproved: %v", err)
	}
	v.Metadata[metaGenerationPlan] = map[string]any{"fingerprint": PlanFingerprint(v), "approved_at": "2026-11-20T08:00:00Z"}
	if err := checkPlanForRender(v); err != nil {
		t.Fatalf("approved: %v", err)
	}
	v.Format = "1080x1350"
	if err := checkPlanForRender(v); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed after approval: %v", err)
	}
}

func TestQuoteCarriesThePlanFingerprint(t *testing.T) {
	v := &ent.CreativeVersion{Format: "1920x1080", Metadata: map[string]any{}, Copy: map[string]any{}}
	q := json.RawMessage(`{"credits_needed":38,"plan":{"scenes":[{"scene_id":"s1","frame":"actor","frame_url":"https://a","prompt":"[VISUAL]"},
		{"scene_id":"s2","frame":"generated","frame_url":"","still_prompt":"kids"}]}}`)
	var out map[string]any
	if err := json.Unmarshal(withPlanFingerprint(q, v), &out); err != nil || out["plan_fingerprint"] != PlanFingerprint(v) || out["plan_approved"] != false || out["credits_needed"] != float64(38) {
		t.Fatalf("quote: %v %v", out, err)
	}
	if missingStill(q) != "s2" {
		t.Fatal("a generated frame without its still must be named")
	}
	scenes := planScenes(q)
	if len(scenes) != 2 || scenes[0].(map[string]any)["prompt"] != "[VISUAL]" {
		t.Fatalf("plan scenes: %v", scenes)
	}
}

// The plan approval is undone by a script edit or a new storyboard, and a redraw replaces the
// brief's actor with a newly drawn one.
func TestPlanApprovalAndActorRedraw(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Kind: VideoPresenter, ActorPrompt: "a warm Kenyan mother"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Format != defaultVideoFormat {
		t.Fatalf("default format: %s", v.Format)
	}
	ai := &fakeJobs{jobs: map[string]string{}, scriptBody: presenterScript, tenant: tid.String()}
	srv := ai.server(t, "svc-key")
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop())
	h := NewHandler(s, nil, nil, zap.NewNop()).WithJobs(r)
	r.Tick(ctx)
	board := func(actor string) string {
		return `{"kind":"storyboard","status":"done","credits_charged":1,"result":{"url":"","key":"","storyboard":{
		"actor":{"url":"` + actor + `","key":"marketflow/t/creative-drafts/posters/a-actor.jpg","prompt":"a warm Kenyan mother","seed":42},
		"stills":[{"scene_id":"s1","url":"` + actor + `","source":"actor"},
		{"scene_id":"s2","url":"https://media.example/s2.jpg","key":"marketflow/t/creative-drafts/posters/a-sb-s2.jpg","source":"generated"},
		{"scene_id":"s3","url":"https://media.example/p.jpg","source":"photo"}],"cost_usd":0.007}}}`
	}
	if _, err := s.MakeStoryboard(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	if code := callback(t, h, v.ID, board("https://media.example/actor.jpg")); code != http.StatusNoContent {
		t.Fatalf("storyboard callback: %d", code)
	}
	if _, err := s.ApproveStoryboard(ctx, tid, v.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovePlan(ctx, tid, v.ID, "not-the-plan", nil, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale fingerprint was approved: %v", err)
	}
	mustApprovePlan(t, s, tid, v.ID)

	// A script edit undoes the plan approval.
	cur, _ := s.GetVersion(ctx, tid, v.ID)
	var script VideoScript
	raw, _ := json.Marshal(cur.Copy["script"])
	_ = json.Unmarshal(raw, &script)
	script.Scenes[0].Voiceover = "Parents, what will the holiday hold?"
	if _, err := s.UpdateScript(ctx, tid, v.ID, ScriptInput{Script: &script}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenderVideoAd(ctx, tid, v.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("rendered after the script changed: %v", err)
	}

	// Redraw: the storyboard is asked for without the brief's actor, and the new one replaces it.
	if _, err := s.MakeStoryboardWith(ctx, tid, v.ID, StoryboardOptions{RedrawActor: true}); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	last := ai.started[len(ai.started)-1]
	if _, kept := last["actor"]; kept || last["actor_prompt"] != "a warm Kenyan mother" {
		t.Fatalf("redraw sent the old actor: %v", last)
	}
	if code := callback(t, h, v.ID, board("https://media.example/actor-2.jpg")); code != http.StatusNoContent {
		t.Fatalf("redraw callback: %d", code)
	}
	brief, _ := s.GetBrief(ctx, tid, b.ID)
	if actor, _ := brief.Metadata["actor"].(map[string]any); actor["url"] != "https://media.example/actor-2.jpg" {
		t.Fatalf("the redrawn actor did not replace the brief's: %v", brief.Metadata["actor"])
	}
	got, _ := s.GetVersion(ctx, tid, v.ID)
	if _, ok := got.Metadata[metaGenerationPlan]; ok || storyboardApproved(got) {
		t.Fatal("a new storyboard must be approved again")
	}
	if _, err := s.MakeStoryboardWith(ctx, tid, v.ID, StoryboardOptions{RedrawActor: true, ActorPrompt: strings.Repeat("x", 301)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long actor prompt: %v", err)
	}
}
