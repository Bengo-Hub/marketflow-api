package worker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/testimonials"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A published good testimonial makes one draft a day for agents that opted in: the agent's next
// rotated offering, recorded as a testimonial run.
func TestTestimonialAds(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "testimonial-ads")
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.AgentRun.Delete().Where(agentrun.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	h := NewTestimonialAdsHandler(db, NewAutopilotRunner(db, nil, "https://mf.example", zap.NewNop()), zap.NewNop())
	var claimMu sync.Mutex
	claimed := map[string]bool{}
	h.claim = func(_ context.Context, key string, _ time.Duration) bool {
		claimMu.Lock()
		defer claimMu.Unlock()
		if claimed[key] {
			return false
		}
		claimed[key] = true
		return true
	}
	a := seedAutopilot(t, db, tid, 100)
	ev := testimonialPublishedEvent{TenantID: tid.String(), TestimonialID: uuid.NewString(), Rating: 5}

	for rating, want := range map[int]bool{0: true, 4: true, 5: true, 3: false, 1: false} {
		if got := (testimonialPublishedEvent{Rating: rating}).good(); got != want {
			t.Fatalf("rating %d: good = %v, want %v", rating, got, want)
		}
	}
	if h.Subject() != testimonials.TestimonialPublishedTopic || h.Subject() != "marketflow.testimonial.published" {
		t.Fatalf("the subject is the outbox one, inside the worker stream's marketflow.> binding: %s", h.Subject())
	}

	// The real envelope decodes, with the tenant taken from it.
	env, _ := json.Marshal(map[string]any{"event_type": "published", "tenant_id": tid.String(),
		"payload": map[string]any{"testimonial_id": ev.TestimonialID, "rating": 5}})
	decoded, err := decodeTestimonialPublished(env)
	if err != nil || decoded.TenantID != tid.String() || decoded.TestimonialID != ev.TestimonialID || !decoded.good() {
		t.Fatalf("envelope: %+v %v", decoded, err)
	}

	if err := h.process(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if n := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).CountX(ctx); n != 0 {
		t.Fatalf("an agent that did not opt in made %d drafts", n)
	}

	meta := a.Metadata
	raw, _ := json.Marshal(meta["autopilot"])
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	cfg["testimonial_drafts"] = true
	meta["autopilot"] = cfg
	db.CustomAgent.UpdateOneID(a.ID).SetMetadata(meta).ExecX(ctx)

	low := ev
	low.Rating = 2
	if err := h.process(ctx, low); err != nil {
		t.Fatal(err)
	}
	if n := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).CountX(ctx); n != 0 {
		t.Fatalf("a low rating made %d drafts", n)
	}

	// Two published at once (a batch of reviews) still make one paid draft.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = h.process(ctx, ev)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).CountX(ctx); n != 1 {
		t.Fatalf("one draft for the day, got %d", n)
	}
	run := db.AgentRun.Query().Where(agentrun.CustomAgentID(a.ID)).OnlyX(ctx)
	if run.Status != "completed" || run.TriggeredBy != triggeredByTestimonial || run.TriggerPayload["testimonial_id"] != ev.TestimonialID {
		t.Fatalf("run: %s %s %v", run.Status, run.TriggeredBy, run.TriggerPayload)
	}

	ev.TestimonialID = uuid.NewString()
	if err := h.process(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if n := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).CountX(ctx); n != 1 {
		t.Fatalf("a second testimonial the same day made a draft: %d", n)
	}
}
