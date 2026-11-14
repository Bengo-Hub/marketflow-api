package custom_agents

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Autopilot agents are a plan feature (owner, 2026-11-14). Creating an ad_autopilot agent (saving
// its settings or hiring an Autopilot template) needs the plan feature marketflow_autopilot, and
// switching one on also needs a free slot under max_autopilot_agents. Both are read strictly from
// subscriptions-api: an outage refuses, and the demo bypass grants nothing. The platform owner's
// tenant (PLATFORM_TENANT) is exempt, as in the Marketing OS plan levels.
//
// MarketFlow Lite 1, Growth 3, Pro 10, Business unlimited; PowerSuite Professional 3, Gold unlimited.

// Plan codes read from subscriptions-api.
const (
	AutopilotFeature     = "marketflow_autopilot"
	AutopilotAgentsLimit = "max_autopilot_agents"
)

// AutopilotUpgradePath is the page the UI sends an owner to for a bigger plan.
const AutopilotUpgradePath = "/settings/billing"

// defaultAutopilotAgents applies when the plan includes Autopilot but carries no limit: the
// smallest paid allowance (MarketFlow Lite).
const defaultAutopilotAgents = 1

// Plan errors. The handler answers 403 (not in the plan), 402 (limit reached) or 503 (the plan
// could not be read), each with upgrade_path in the details.
var (
	ErrAutopilotNotInPlan   = errors.New("autopilot agents are not in your plan. Upgrade to MarketFlow Lite or higher to use them")
	ErrAutopilotLimit       = errors.New("autopilot agent limit reached")
	ErrAutopilotPlanUnknown = errors.New("your plan could not be checked just now, try again shortly")
)

// AutopilotLimitError says how many agents the plan allows and how many are on.
type AutopilotLimitError struct {
	Limit  int
	Active int
}

func (e *AutopilotLimitError) Error() string {
	return fmt.Sprintf("your plan allows %d active Autopilot agent(s) and %d are on. Switch one off or upgrade for more", e.Limit, e.Active)
}

// Is makes errors.Is(err, ErrAutopilotLimit) match.
func (e *AutopilotLimitError) Is(target error) bool { return target == ErrAutopilotLimit }

// WithAutopilotPlan wires the strict plan reads (subscriptions StrictFeature and StrictLimit).
// Without it no tenant but the platform's may create or switch on an Autopilot agent.
func WithAutopilotPlan(feature func(ctx context.Context, tenantID, feature string) (has, known bool),
	limit func(ctx context.Context, tenantID, key string) (n int, has, known bool)) func(*Service) {
	return func(s *Service) {
		s.planFeature, s.planLimit = feature, limit
	}
}

// AutopilotPlan is what the Autopilot page shows about the plan: whether it includes the agents,
// how many may be on (-1 unlimited) and how many are.
type AutopilotPlan struct {
	Included    bool   `json:"included"`
	Unknown     bool   `json:"unknown,omitempty"`
	Exempt      bool   `json:"exempt,omitempty"`
	Limit       int    `json:"limit"`
	Active      int    `json:"active"`
	CanActivate bool   `json:"can_activate"`
	UpgradePath string `json:"upgrade_path"`
}

// isAdAutopilot matches agents whose metadata.kind is ad_autopilot.
func isAdAutopilot() predicate.CustomAgent {
	return func(s *sql.Selector) {
		s.Where(sql.P(func(b *sql.Builder) {
			b.WriteString("COALESCE(" + s.C(customagent.FieldMetadata) + "->>'kind', '') = ").Arg(KindAdAutopilot)
		}))
	}
}

// isAutopilotAgent reports whether the agent is an ad autopilot.
func isAutopilotAgent(a *ent.CustomAgent) bool {
	kind, _ := a.Metadata["kind"].(string)
	return kind == KindAdAutopilot
}

// isPlatformTenant reports whether tenantID is the platform owner's tenant.
func (s *Service) isPlatformTenant(ctx context.Context, tenantID uuid.UUID) bool {
	pid, err := s.platformID(ctx)
	return err == nil && pid == tenantID
}

// activeAutopilots counts the tenant's switched-on ad autopilots, leaving out agentID.
func (s *Service) activeAutopilots(ctx context.Context, tenantID, agentID uuid.UUID) (int, error) {
	q := s.client.CustomAgent.Query().
		Where(customagent.TenantID(tenantID), customagent.IsActive(true), isAdAutopilot(), notTemplate())
	if agentID != uuid.Nil {
		q = q.Where(customagent.IDNEQ(agentID))
	}
	return q.Count(ctx)
}

// autopilotLimit is max_autopilot_agents (-1 unlimited), or defaultAutopilotAgents when the plan
// sets none. An answer that is not definite is ErrAutopilotPlanUnknown.
func (s *Service) autopilotLimit(ctx context.Context, tenantID uuid.UUID) (int, error) {
	if s.planLimit == nil {
		return defaultAutopilotAgents, nil
	}
	n, has, known := s.planLimit(ctx, tenantID.String(), AutopilotAgentsLimit)
	switch {
	case !known:
		return 0, ErrAutopilotPlanUnknown
	case !has:
		return defaultAutopilotAgents, nil
	}
	return n, nil
}

// checkAutopilot refuses an Autopilot agent the plan does not allow. activating also checks that
// switching agentID on keeps the tenant within max_autopilot_agents.
func (s *Service) checkAutopilot(ctx context.Context, tenantID, agentID uuid.UUID, activating bool) error {
	if s.isPlatformTenant(ctx, tenantID) {
		return nil
	}
	if s.planFeature == nil {
		return ErrAutopilotNotInPlan
	}
	has, known := s.planFeature(ctx, tenantID.String(), AutopilotFeature)
	if !known {
		return ErrAutopilotPlanUnknown
	}
	if !has {
		return ErrAutopilotNotInPlan
	}
	if !activating {
		return nil
	}
	limit, err := s.autopilotLimit(ctx, tenantID)
	if err != nil {
		return err
	}
	if limit < 0 {
		return nil
	}
	active, err := s.activeAutopilots(ctx, tenantID, agentID)
	if err != nil {
		return fmt.Errorf("count autopilot agents: %w", err)
	}
	if active >= limit {
		return &AutopilotLimitError{Limit: limit, Active: active}
	}
	return nil
}

// AutopilotPlanStatus reads the plan's Autopilot allowance and the agents switched on.
func (s *Service) AutopilotPlanStatus(ctx context.Context, tenantID uuid.UUID) (AutopilotPlan, error) {
	st := AutopilotPlan{UpgradePath: AutopilotUpgradePath}
	active, err := s.activeAutopilots(ctx, tenantID, uuid.Nil)
	if err != nil {
		return st, fmt.Errorf("count autopilot agents: %w", err)
	}
	st.Active = active
	if s.isPlatformTenant(ctx, tenantID) {
		st.Included, st.Exempt, st.Limit, st.CanActivate = true, true, -1, true
		return st, nil
	}
	if s.planFeature != nil {
		has, known := s.planFeature(ctx, tenantID.String(), AutopilotFeature)
		st.Included, st.Unknown = has && known, !known
	}
	if !st.Included {
		return st, nil
	}
	limit, err := s.autopilotLimit(ctx, tenantID)
	if err != nil {
		st.Unknown = true
		return st, nil
	}
	st.Limit = limit
	st.CanActivate = limit < 0 || active < limit
	return st, nil
}

// autopilotPlanReply writes the answer for a plan error and reports whether err was one.
func autopilotPlanReply(w http.ResponseWriter, err error) bool {
	details := map[string]any{"feature": AutopilotFeature, "upgrade_path": AutopilotUpgradePath}
	var lim *AutopilotLimitError
	switch {
	case errors.As(err, &lim):
		details["limit_key"], details["limit"], details["active"] = AutopilotAgentsLimit, lim.Limit, lim.Active
		httpapi.Error(w, http.StatusPaymentRequired, "autopilot_limit_reached", lim.Error(), details)
	case errors.Is(err, ErrAutopilotNotInPlan):
		httpapi.Error(w, http.StatusForbidden, "autopilot_not_in_plan", err.Error(), details)
	case errors.Is(err, ErrAutopilotPlanUnknown):
		httpapi.Error(w, http.StatusServiceUnavailable, "plan_unavailable", err.Error(), details)
	default:
		return false
	}
	return true
}

// GetAutopilotPlan handles GET /api/v1/agents/autopilot/plan.
// @Summary The plan's Autopilot allowance: included, limit (-1 unlimited) and agents switched on
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} AutopilotPlan
// @Security bearerAuth
// @Router /agents/autopilot/plan [get]
func (h *Handler) GetAutopilotPlan(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	st, err := h.service.AutopilotPlanStatus(r.Context(), tenantID)
	if err != nil {
		h.logger.Error("autopilot plan", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "plan_failed", "failed to read the Autopilot plan", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, st)
}
