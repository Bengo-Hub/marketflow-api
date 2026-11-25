package custom_agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Owner 2026-11-25: only an admin changes, switches or deletes an Autopilot agent; a plain agent
// stays a member's work.
func TestAutopilotAgentChangesAreAdminOnly(t *testing.T) {
	db := openPlanTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	mk := func(meta map[string]any) uuid.UUID {
		a := db.CustomAgent.Create().SetTenantID(tid).SetName("A").SetTriggerType("manual").SetIsActive(false).SetMetadata(meta).SaveX(ctx)
		t.Cleanup(func() { _ = db.CustomAgent.DeleteOneID(a.ID).Exec(ctx) })
		return a.ID
	}
	autopilot, plain := mk(map[string]any{"kind": KindAdAutopilot}), mk(map[string]any{"kind": KindCRM})
	admin := false
	h := NewHandler(NewService(db, zap.NewNop()), zap.NewNop())
	h.CanAdmin = func(context.Context, uuid.UUID) bool { return admin }
	r := chi.NewRouter()
	r.Put("/{id}", h.Update)
	r.Delete("/{id}", h.Delete)
	call := func(method string, id uuid.UUID) int {
		req := httptest.NewRequest(method, "/"+id.String(), strings.NewReader(`{"is_active":false}`))
		req.Header.Set("X-Tenant-ID", tid.String())
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		if got := call(m, autopilot); got != http.StatusForbidden {
			t.Errorf("member %s autopilot: %d", m, got)
		}
		if got := call(m, plain); got != http.StatusOK {
			t.Errorf("member %s plain agent: %d", m, got)
		}
		if got := call(m, uuid.New()); got != http.StatusNotFound {
			t.Errorf("member %s unknown agent: %d", m, got)
		}
	}
	admin = true
	if got := call(http.MethodPut, autopilot); got != http.StatusOK {
		t.Errorf("admin: %d", got)
	}
}
