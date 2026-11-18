package onboarding

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
)

// Quick start steps (Sprint 6): choose a plan, add the chat widget to the business's website and
// share the public page. They are optional, so a tenant who finished setup before they existed
// keeps a complete checklist. Each is done from data where data can tell, else when the owner
// marks it done in the wizard.
const (
	StepPlan      = "plan"
	StepWidget    = "widget"
	StepSharePage = "share_page"

	// StepsMetadataKey holds the steps the owner marked done, {key: RFC 3339 time}. Its own key in
	// the tenant metadata, so it never races the dismiss state.
	StepsMetadataKey = "onboarding_steps"

	// widgetLeadSource is the source of a lead the chat widget's form sent (webhooks public_handler).
	widgetLeadSource = "widget"
)

// markableSteps are the steps the owner can mark done. The others are done only from data.
var markableSteps = map[string]bool{StepPlan: true, StepWidget: true, StepSharePage: true}

// ErrUnknownStep is a step the owner cannot mark done.
var ErrUnknownStep = errors.New("unknown quick start step")

// WithPlanStatus lets the plan step read the tenant's subscription status (the subscriptions
// snapshot's status, such as ACTIVE or TRIAL). known is false when it could not be read; the step
// then waits for the owner to mark it.
func (s *Service) WithPlanStatus(status func(ctx context.Context, tenantID string) (status string, known bool)) *Service {
	s.planStatus = status
	return s
}

// markedDone reports whether the owner marked a step done.
func markedDone(meta map[string]any, key string) bool {
	steps, ok := meta[StepsMetadataKey].(map[string]any)
	if !ok {
		return false
	}
	at, ok := steps[key].(string)
	return ok && at != ""
}

// quickStartSteps are the plan, widget and share steps of a tenant's checklist.
func (s *Service) quickStartSteps(ctx context.Context, tenantID uuid.UUID, meta map[string]any) (plan Step, widget Step, share Step) {
	// A paid plan, or the owner chose to stay on the trial for now.
	planDone := markedDone(meta, StepPlan)
	if !planDone && s.planStatus != nil {
		if st, known := s.planStatus(ctx, tenantID.String()); known {
			planDone = strings.EqualFold(st, "ACTIVE")
		}
	}
	// A lead from the widget's form proves it is on a live site; the owner can also mark it.
	widgetDone := markedDone(meta, StepWidget)
	if !widgetDone {
		widgetDone, _ = s.db.Lead.Query().Where(lead.TenantID(tenantID), lead.Source(widgetLeadSource)).Exist(ctx)
	}
	return Step{Key: StepPlan, Done: planDone, Optional: true},
		Step{Key: StepWidget, Done: widgetDone, Optional: true},
		Step{Key: StepSharePage, Done: markedDone(meta, StepSharePage), Optional: true}
}

// MarkStepDone records that the owner did a quick start step (chose to stay on the trial, added
// the widget, shared the page). Marking it again keeps the first time.
func (s *Service) MarkStepDone(ctx context.Context, tenantID uuid.UUID, key string) error {
	if !markableSteps[key] {
		return ErrUnknownStep
	}
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("load tenant: %w", err)
	}
	steps := map[string]any{}
	if cur, ok := t.Metadata[StepsMetadataKey].(map[string]any); ok {
		for k, v := range cur {
			steps[k] = v
		}
	}
	if at, ok := steps[key].(string); ok && at != "" {
		return nil
	}
	steps[key] = time.Now().UTC().Format(time.RFC3339)
	return metastore.SetKeys(ctx, s.db, tenantID, map[string]any{StepsMetadataKey: steps})
}

// MarkStep handles POST /api/v1/tenant/onboarding/steps/{key}/done: the owner did a quick start
// step (plan, widget or share_page). 204; 400 for any other step.
// @Summary Mark a quick start step done
// @Tags Onboarding
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param key path string true "plan, widget or share_page"
// @Success 204
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/onboarding/steps/{key}/done [post]
func (h *Handler) MarkStep(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantID(r)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	err := h.svc.MarkStepDone(r.Context(), tid, chi.URLParam(r, "key"))
	switch {
	case errors.Is(err, ErrUnknownStep):
		httpapi.Error(w, http.StatusBadRequest, "unknown_step", "only plan, widget and share_page can be marked done", nil)
		return
	case err != nil:
		h.logger.Error("mark step", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
