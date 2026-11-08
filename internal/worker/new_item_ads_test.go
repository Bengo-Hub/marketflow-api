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
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A new item makes one draft a day for agents that opted in, and only for an item an ad can show.
func TestNewItemAds(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "new-item-ads")
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_, _ = db.AgentRun.Delete().Where(agentrun.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	h := NewNewItemAdsHandler(db, NewAutopilotRunner(db, nil, "https://mf.example", zap.NewNop()), zap.NewNop())
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
	ev := itemCreatedEvent{TenantID: tid.String(), ID: uuid.NewString(), Name: "Toaster", Type: "GOODS", IsActive: true, ImageURL: "https://media.example/toaster.jpg"}
	if !ev.advertisable() {
		t.Fatal("a plain item with a photo can be advertised")
	}
	for name, bad := range map[string]func(*itemCreatedEvent){
		"no photo":     func(e *itemCreatedEvent) { e.ImageURL = "" },
		"inactive":     func(e *itemCreatedEvent) { e.IsActive = false },
		"not for sale": func(e *itemCreatedEvent) { e.NotForSale = true },
		"ingredient":   func(e *itemCreatedEvent) { e.Type = "INGREDIENT" },
		"age limited":  func(e *itemCreatedEvent) { e.AgeLimited = true },
		"controlled":   func(e *itemCreatedEvent) { e.Controlled = true },
	} {
		e := ev
		bad(&e)
		if e.advertisable() {
			t.Fatalf("%s must not make a draft", name)
		}
	}

	// The real envelope decodes, with the tenant taken from it.
	env, _ := json.Marshal(map[string]any{"event_type": "item.created", "tenant_id": tid.String(),
		"payload": map[string]any{"id": ev.ID, "name": ev.Name, "type": "GOODS", "is_active": true, "image_url": ev.ImageURL}})
	decoded, err := decodeItemCreated(env)
	if err != nil || decoded.TenantID != tid.String() || !decoded.advertisable() {
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
	cfg["new_item_drafts"] = true
	meta["autopilot"] = cfg
	db.CustomAgent.UpdateOneID(a.ID).SetMetadata(meta).ExecX(ctx)
	// A bulk import delivers items together: two at once still make one paid draft.
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
	briefs := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).AllX(ctx)
	if len(briefs) != 1 || briefs[0].Offering["name"] != "Toaster" {
		t.Fatalf("the draft must be for the new item: %+v", briefs)
	}
	run := db.AgentRun.Query().Where(agentrun.CustomAgentID(a.ID)).OnlyX(ctx)
	if run.Status != "completed" || run.TriggeredBy != triggeredByNewItem {
		t.Fatalf("run: %s %s", run.Status, run.TriggeredBy)
	}

	ev.ID, ev.Name = uuid.NewString(), "Kettle"
	if err := h.process(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if n := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).CountX(ctx); n != 1 {
		t.Fatalf("a second item the same day made a draft: %d", n)
	}
}
