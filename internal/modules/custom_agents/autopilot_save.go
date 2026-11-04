package custom_agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

var errAgentGone = errors.New("custom agent not found")

// MaxAutopilotVersions is how much config history an agent keeps for rollback.
const MaxAutopilotVersions = 20

// AutopilotInput is the body of PUT /agents/custom/{id}/autopilot. Offerings are the catalogue
// snapshots of config.offering_ids, in the same order, as the offering picker returned them.
type AutopilotInput struct {
	Config    AutopilotConfig  `json:"config"`
	Offerings []map[string]any `json:"offerings"`
}

// configVersion is one saved config, kept in metadata.versions for rollback.
type configVersion struct {
	Version   int              `json:"version"`
	SavedAt   string           `json:"saved_at"`
	Config    AutopilotConfig  `json:"config"`
	Offerings []map[string]any `json:"offerings"`
}

// SaveAutopilot validates the config, makes the agent an ad_autopilot on the config's schedule
// and adds a config version. Auto-publish is refused for now: its veto window and the Autopilot
// terms are not built, and a run ends at a draft either way.
func (s *Service) SaveAutopilot(ctx context.Context, tenantID, agentID uuid.UUID, in AutopilotInput) (*ent.CustomAgent, error) {
	if err := in.Config.Validate(false); err != nil {
		return nil, err
	}
	if len(in.Offerings) != len(in.Config.OfferingIDs) {
		return nil, fmt.Errorf("%w: each offering needs its catalogue details", ErrInvalid)
	}
	a, err := s.Get(ctx, tenantID, agentID)
	if err != nil {
		return nil, err
	}
	versions := readVersions(a)
	next := 1
	if n := len(versions); n > 0 {
		next = versions[n-1].Version + 1
	}
	versions = append(versions, configVersion{Version: next, SavedAt: time.Now().UTC().Format(time.RFC3339), Config: in.Config, Offerings: in.Offerings})
	if len(versions) > MaxAutopilotVersions {
		versions = versions[len(versions)-MaxAutopilotVersions:]
	}
	return s.applyAutopilot(ctx, tenantID, agentID, in.Config, in.Offerings, versions)
}

// RollbackAutopilot restores a saved version as a new version, so the history stays complete.
func (s *Service) RollbackAutopilot(ctx context.Context, tenantID, agentID uuid.UUID, version int) (*ent.CustomAgent, error) {
	a, err := s.Get(ctx, tenantID, agentID)
	if err != nil {
		return nil, err
	}
	versions := readVersions(a)
	for _, v := range versions {
		if v.Version == version {
			return s.SaveAutopilot(ctx, tenantID, agentID, AutopilotInput{Config: v.Config, Offerings: v.Offerings})
		}
	}
	return nil, fmt.Errorf("%w: version %d is not in this agent's history", ErrInvalid, version)
}

// applyAutopilot writes the config in one statement: the schedule columns plus a jsonb merge of
// the autopilot keys, so other metadata keys (template_ref) stay.
func (s *Service) applyAutopilot(ctx context.Context, tenantID, agentID uuid.UUID, cfg AutopilotConfig, offerings []map[string]any, versions []configVersion) (*ent.CustomAgent, error) {
	raw, err := json.Marshal(map[string]any{"kind": KindAdAutopilot, "autopilot": cfg, "autopilot_offerings": offerings, "versions": versions})
	if err != nil {
		return nil, err
	}
	value := sql.ExprFunc(func(b *sql.Builder) {
		b.WriteString("(COALESCE(" + customagent.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(raw))
		b.WriteString("::jsonb)")
	})
	trigger := map[string]any{"frequency": cfg.Cadence, "time": cfg.Time}
	if cfg.Cadence == "weekly" {
		trigger["weekday"] = float64(cfg.Weekday)
	}
	if err := ValidateTrigger(TriggerSchedule, trigger); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	n, err := s.client.CustomAgent.Update().
		Where(customagent.IDEQ(agentID), customagent.TenantID(tenantID)).
		SetTriggerType(TriggerSchedule).
		SetTriggerConfig(trigger).
		Modify(func(u *sql.UpdateBuilder) { u.Set(customagent.FieldMetadata, value) }).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("save autopilot: %w", err)
	}
	if n == 0 {
		return nil, errAgentGone
	}
	return s.Get(ctx, tenantID, agentID)
}

func readVersions(a *ent.CustomAgent) []configVersion {
	var out []configVersion
	if raw, err := json.Marshal(a.Metadata["versions"]); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// PutAutopilot handles PUT /api/v1/agents/custom/{id}/autopilot.
// @Summary Save an ad autopilot's settings as a new config version
// @Tags Custom Agents
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Agent UUID"
// @Param request body AutopilotInput true "Autopilot settings"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /agents/custom/{id}/autopilot [put]
func (h *Handler) PutAutopilot(w http.ResponseWriter, r *http.Request) {
	tenantID, id, ok := h.autopilotParams(w, r)
	if !ok {
		return
	}
	var in AutopilotInput
	if err := httpapi.DecodeJSON(r, &in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	a, err := h.service.SaveAutopilot(r.Context(), tenantID, id, in)
	h.autopilotReply(w, a, err)
}

// RollbackAutopilot handles POST /api/v1/agents/custom/{id}/autopilot/rollback {version}.
// @Summary Restore an earlier autopilot config
// @Tags Custom Agents
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Agent UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /agents/custom/{id}/autopilot/rollback [post]
func (h *Handler) RollbackAutopilot(w http.ResponseWriter, r *http.Request) {
	tenantID, id, ok := h.autopilotParams(w, r)
	if !ok {
		return
	}
	var in struct {
		Version int `json:"version"`
	}
	if err := httpapi.DecodeJSON(r, &in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	a, err := h.service.RollbackAutopilot(r.Context(), tenantID, id, in.Version)
	h.autopilotReply(w, a, err)
}

func (h *Handler) autopilotParams(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, id, true
}

func (h *Handler) autopilotReply(w http.ResponseWriter, a *ent.CustomAgent, err error) {
	switch {
	case err == nil:
		httpapi.JSON(w, http.StatusOK, a)
	case errors.Is(err, ErrInvalid):
		httpapi.Error(w, http.StatusBadRequest, "invalid_autopilot", err.Error(), nil)
	case errors.Is(err, ErrAutoPublishNotAllowed):
		httpapi.Error(w, http.StatusForbidden, "auto_publish_not_allowed", err.Error(), nil)
	case ent.IsNotFound(err) || errors.Is(err, errAgentGone):
		httpapi.Error(w, http.StatusNotFound, "not_found", "agent not found", nil)
	default:
		h.logger.Error("save autopilot", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "failed to save the autopilot", nil)
	}
}
