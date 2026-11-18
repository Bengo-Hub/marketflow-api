package onboarding

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
)

// The quick start steps are optional (a finished setup stays finished), each is done from data
// where data can tell (a paid plan, a lead from the widget's form) or when the owner marks it,
// and marking keeps the first time and never touches the dismiss state.
func TestQuickStartSteps(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("QS " + tid.String()).SetSlug("qs-" + tid.String()).
		SetBusinessType("generic").Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	status, known := "TRIAL", true
	svc := NewService(db, marketingos.NewService(db, nil, zap.NewNop()), nil, nil, nil, "codevertex", zap.NewNop()).
		WithPlanStatus(func(context.Context, string) (string, bool) { return status, known })

	c, err := svc.Checklist(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Steps[0].Key != StepPlan {
		t.Fatalf("the plan step comes first: %+v", c.Steps)
	}
	for _, key := range []string{StepPlan, StepWidget, StepSharePage} {
		found := false
		for _, s := range c.Steps {
			if s.Key == key {
				found = true
				if !s.Optional || s.Done {
					t.Errorf("%s: %+v", key, s)
				}
			}
		}
		if !found {
			t.Errorf("no %s step: %+v", key, c.Steps)
		}
	}
	if c.Total != 5 {
		t.Fatalf("the quick start steps must not count as required: %+v", c)
	}

	// A paid plan completes the plan step; an unreadable status leaves it to the owner.
	status = "ACTIVE"
	if c, _ = svc.Checklist(ctx, tid); !stepDone(c, StepPlan) {
		t.Fatal("ACTIVE must complete the plan step")
	}
	status, known = "", false
	if c, _ = svc.Checklist(ctx, tid); stepDone(c, StepPlan) {
		t.Fatal("an unknown status must not complete the plan step")
	}

	// A lead from the widget's form completes the widget step; another source does not.
	db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetStatus("new").ExecX(ctx)
	if c, _ = svc.Checklist(ctx, tid); stepDone(c, StepWidget) {
		t.Fatal("a website lead is not proof of the widget")
	}
	db.Lead.Create().SetTenantID(tid).SetSource(widgetLeadSource).SetLeadType("customer").SetStatus("new").ExecX(ctx)
	if c, _ = svc.Checklist(ctx, tid); !stepDone(c, StepWidget) {
		t.Fatal("a widget lead must complete the widget step")
	}

	// The owner marks the plan (stay on the trial) and the share step done.
	if err := svc.SetDismissed(ctx, tid, true); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{StepPlan, StepSharePage} {
		if err := svc.MarkStepDone(ctx, tid, key); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := db.Tenant.Get(ctx, tid)
	firstAt := first.Metadata[StepsMetadataKey].(map[string]any)[StepPlan]
	if err := svc.MarkStepDone(ctx, tid, StepPlan); err != nil {
		t.Fatal(err)
	}
	again, _ := db.Tenant.Get(ctx, tid)
	if again.Metadata[StepsMetadataKey].(map[string]any)[StepPlan] != firstAt {
		t.Fatal("marking again must keep the first time")
	}
	c, _ = svc.Checklist(ctx, tid)
	if !stepDone(c, StepPlan) || !stepDone(c, StepSharePage) || !c.Dismissed {
		t.Fatalf("after marking: %+v", c)
	}
	for _, key := range []string{"profile", "lead_capture", "", "team"} {
		if err := svc.MarkStepDone(ctx, tid, key); !errors.Is(err, ErrUnknownStep) {
			t.Errorf("%q: %v", key, err)
		}
	}

	// Through the handler.
	h := NewHandler(svc, zap.NewNop())
	r := chi.NewRouter()
	r.Post("/tenant/onboarding/steps/{key}/done", h.MarkStep)
	post := func(key string) int {
		req := httptest.NewRequest(http.MethodPost, "/tenant/onboarding/steps/"+key+"/done", nil)
		req.Header.Set("X-Tenant-ID", tid.String())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(StepWidget); code != http.StatusNoContent {
		t.Fatalf("mark widget: %d", code)
	}
	if code := post("profile"); code != http.StatusBadRequest {
		t.Fatalf("mark profile: %d", code)
	}
}
