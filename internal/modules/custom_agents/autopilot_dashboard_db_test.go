package custom_agents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
)

// The dashboard lists only this agent's drafts that wait for the owner (a script to review or a
// finished ad to approve), never an approved one, a veto-window video the agent carries on by
// itself, or another agent's draft; and it answers 404 for an agent that is not an ad autopilot
// or belongs to another tenant.
func TestAutopilotDashboard(t *testing.T) {
	db := openPlanTestDB(t)
	ctx := context.Background()
	tid := planTenant(t, db, "dash-"+uuid.NewString()[:8])
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
	})

	agent := db.CustomAgent.Create().SetTenantID(tid).SetName("Weekly ads").SetTriggerType(TriggerSchedule).
		SetMetadata(map[string]any{"kind": KindAdAutopilot}).SaveX(ctx)
	otherAgent := db.CustomAgent.Create().SetTenantID(tid).SetName("Other ads").SetTriggerType(TriggerSchedule).
		SetMetadata(map[string]any{"kind": KindAdAutopilot}).SaveX(ctx)
	plain := plainAgent(t, db, tid, "Not an autopilot")

	brief := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Blender week 1").
		SetMetadata(map[string]any{"autopilot_agent_id": agent.ID.String()}).SaveX(ctx)
	otherBrief := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Other").
		SetMetadata(map[string]any{"autopilot_agent_id": otherAgent.ID.String()}).SaveX(ctx)
	version := func(b *ent.CreativeBrief, no int, status string, meta map[string]any) *ent.CreativeVersion {
		c := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(no).SetKind("video_ad").
			SetFormat("1080x1920").SetStatus(status).SetCredits(3)
		if meta != nil {
			c.SetMetadata(meta)
		}
		return c.SaveX(ctx)
	}
	script := version(brief, 1, draftScriptReady, nil)
	ready := version(brief, 2, draftReady, nil)
	version(brief, 3, "approved", nil)
	version(brief, 4, draftReady, map[string]any{"autopilot": map[string]any{"mode": "veto_window", "state": "active"}})
	// A veto-window video the owner stopped is back with the owner.
	stopped := version(brief, 5, draftReady, map[string]any{"autopilot": map[string]any{"mode": "veto_window", "state": "vetoed"}})
	version(otherBrief, 1, draftScriptReady, nil)

	svc := NewService(db, zap.NewNop())
	d, err := svc.AutopilotDashboard(ctx, tid, agent.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, p := range d.Pending {
		got[p.VersionID] = true
		if p.Title != "Blender week 1" || p.BriefID != brief.ID {
			t.Errorf("pending draft %s: title %q brief %s", p.VersionID, p.Title, p.BriefID)
		}
	}
	if len(d.Pending) != 3 || !got[script.ID] || !got[ready.ID] || !got[stopped.ID] {
		t.Fatalf("pending: %+v", d.Pending)
	}
	// Five versions of this agent made today, 3 credits each; the other agent's are not counted.
	if d.Week.AdsMade != 5 || d.Week.Credits != 15 {
		t.Fatalf("week: %+v", d.Week)
	}

	if _, err := svc.AutopilotDashboard(ctx, tid, plain.ID, time.Now()); !errors.Is(err, errNotAutopilot) {
		t.Fatalf("plain agent: %v", err)
	}
	if _, err := svc.AutopilotDashboard(ctx, uuid.New(), agent.ID, time.Now()); !ent.IsNotFound(err) {
		t.Fatalf("another tenant's agent: %v", err)
	}

	// Through the handler: 200 with the read, 404 for a plain agent.
	h := NewHandler(svc, zap.NewNop())
	r := chi.NewRouter()
	r.Get("/agents/custom/{id}/autopilot/dashboard", h.GetAutopilotDashboard)
	get := func(id uuid.UUID) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/agents/custom/"+id.String()+"/autopilot/dashboard", nil)
		req.Header.Set("X-Tenant-ID", tid.String())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	rec := get(agent.ID)
	var body AutopilotDashboard
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Pending) != 3 {
		t.Fatalf("handler: %d %s", rec.Code, rec.Body)
	}
	if rec := get(plain.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("plain agent through the handler: %d %s", rec.Code, rec.Body)
	}
}

// An agent with no briefs yet has an empty list, not null, and a zero week.
func TestAutopilotDashboardEmpty(t *testing.T) {
	db := openPlanTestDB(t)
	ctx := context.Background()
	tid := planTenant(t, db, "dash-empty-"+uuid.NewString()[:8])
	agent := db.CustomAgent.Create().SetTenantID(tid).SetName("New").SetTriggerType(TriggerSchedule).
		SetMetadata(map[string]any{"kind": KindAdAutopilot}).SaveX(ctx)
	d, err := NewService(db, zap.NewNop()).AutopilotDashboard(ctx, tid, agent.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if d.Pending == nil || len(d.Pending) != 0 || d.Week.AdsMade != 0 {
		t.Fatalf("empty: %+v", d)
	}
	b, _ := json.Marshal(d)
	if !json.Valid(b) || string(b[:12]) != `{"pending":[` {
		t.Fatalf("pending must be a list: %s", b)
	}
}
