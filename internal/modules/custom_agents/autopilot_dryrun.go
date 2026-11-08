package custom_agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// OutputScript is the registry output one autopilot run pays for: a run ends at a script draft.
const OutputScript = "script"

// tierOutput maps a video tier onto the registry output the owner pays for when rendering it.
var tierOutput = map[string]string{"template": "template_video_30s", "presenter": "presenter_30s", "presenter_premium": "presenter_premium_30s"}

// Most runs a calendar month can hold, so a forecast is never below what a month can really cost.
const (
	maxDailyRuns  = 31
	maxWeeklyRuns = 5
)

// CreditForecast is marketflow-ai's answer to what one output costs now. Balance is -1 when it is
// unlimited or could not be read.
type CreditForecast struct {
	CreditsNeeded int  `json:"credits_needed"`
	Balance       int  `json:"balance"`
	Enough        bool `json:"enough"`
}

// CreditForecaster prices a registry output against the tenant's balance. Nothing is spent.
type CreditForecaster func(ctx context.Context, tenantID uuid.UUID, output string) (CreditForecast, error)

// NewCreditForecaster asks marketflow-ai, the one owner of the subscriptions-api credit client.
// It returns nil when the service is not configured.
func NewCreditForecaster(aiURL, key string) CreditForecaster {
	if aiURL == "" || key == "" {
		return nil
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	url := strings.TrimSuffix(aiURL, "/") + "/api/v1/internal/credits/forecast"
	return func(ctx context.Context, tenantID uuid.UUID, output string) (CreditForecast, error) {
		var fc CreditForecast
		body, _ := json.Marshal(map[string]string{"tenant_id": tenantID.String(), "output": output})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fc, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", key)
		resp, err := hc.Do(req)
		if err != nil {
			return fc, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fc, fmt.Errorf("credit forecast: status %d", resp.StatusCode)
		}
		return fc, json.NewDecoder(resp.Body).Decode(&fc)
	}
}

// WithCreditForecaster wires the prices the dry run shows.
func WithCreditForecaster(f CreditForecaster) func(*Service) {
	return func(s *Service) { s.forecaster = f }
}

// ReadAutopilot reads an agent's saved config and offering snapshots.
func ReadAutopilot(a *ent.CustomAgent) (AutopilotConfig, []map[string]any, error) {
	var cfg AutopilotConfig
	raw, err := json.Marshal(a.Metadata["autopilot"])
	if err != nil {
		return cfg, nil, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, nil, fmt.Errorf("the autopilot settings could not be read: %w", err)
	}
	var offerings []map[string]any
	if raw, err := json.Marshal(a.Metadata["autopilot_offerings"]); err == nil {
		_ = json.Unmarshal(raw, &offerings)
	}
	return cfg, offerings, nil
}

// NextRunNumber counts the agent's finished runs, so a failed or skipped run retries the same
// offering.
func NextRunNumber(ctx context.Context, db *ent.Client, agentID uuid.UUID) (int, error) {
	return db.AgentRun.Query().Where(agentrun.CustomAgentID(agentID), agentrun.StatusEQ("completed")).Count(ctx)
}

// DryRun previews an agent's next run and what a month of runs can cost. It makes nothing and
// spends nothing. Credit fields are zero when CreditsKnown is false.
type DryRun struct {
	Offering     string `json:"offering"`
	Language     string `json:"language"`
	Tier         string `json:"tier"`
	Objective    string `json:"objective"`
	RunsPerMonth int    `json:"runs_per_month"` // the most a calendar month can hold

	CreditsKnown  bool `json:"credits_known"`
	ScriptCredits int  `json:"script_credits"` // what each run costs
	RenderCredits int  `json:"render_credits"` // what rendering a draft costs, for the dearest tier
	Balance       int  `json:"balance"`        // -1: unlimited or unknown

	MonthlyDraftCredits int `json:"monthly_draft_credits"` // every run, drafts only
	MonthlyFullCredits  int `json:"monthly_full_credits"`  // every run, every draft rendered
	CreditCeiling       int `json:"credit_ceiling"`
	DraftsWithinCeiling int `json:"drafts_within_ceiling"`
	AdsWithinCeiling    int `json:"ads_within_ceiling"` // drafts rendered too
}

// BuildDryRun fills the month's sums from the prices.
func BuildDryRun(cfg AutopilotConfig, b RunBrief, scriptCredits, renderCredits, balance int, known bool) DryRun {
	name, _ := b.Offering["name"].(string)
	d := DryRun{Offering: name, Language: b.Language, Tier: b.Tier, Objective: cfg.Objective, RunsPerMonth: maxWeeklyRuns, CreditCeiling: cfg.CreditCap, Balance: -1}
	if cfg.Cadence == "daily" {
		d.RunsPerMonth = maxDailyRuns
	}
	if !known {
		return d
	}
	d.CreditsKnown, d.ScriptCredits, d.RenderCredits, d.Balance = true, scriptCredits, renderCredits, balance
	d.MonthlyDraftCredits = d.RunsPerMonth * scriptCredits
	d.MonthlyFullCredits = d.RunsPerMonth * (scriptCredits + renderCredits)
	if scriptCredits > 0 {
		d.DraftsWithinCeiling = min(d.RunsPerMonth, cfg.CreditCap/scriptCredits)
	}
	if full := scriptCredits + renderCredits; full > 0 {
		d.AdsWithinCeiling = min(d.RunsPerMonth, cfg.CreditCap/full)
	}
	return d
}

// DryRunAutopilot previews the agent's next run. Prices that cannot be read leave the preview
// usable, with CreditsKnown false.
func (s *Service) DryRunAutopilot(ctx context.Context, tenantID, agentID uuid.UUID) (DryRun, error) {
	a, err := s.Get(ctx, tenantID, agentID)
	if err != nil {
		return DryRun{}, err
	}
	if kind, _ := a.Metadata["kind"].(string); kind != KindAdAutopilot {
		return DryRun{}, fmt.Errorf("%w: choose this agent's products and schedule first", ErrInvalid)
	}
	cfg, offerings, err := ReadAutopilot(a)
	if err != nil {
		return DryRun{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := cfg.Validate(cfg.Approval == AutoPublish); err != nil {
		return DryRun{}, err
	}
	n, err := NextRunNumber(ctx, s.client, a.ID)
	if err != nil {
		return DryRun{}, err
	}
	b, err := NextBrief(a.ID.String(), cfg, offerings, n, time.Now())
	if err != nil {
		return DryRun{}, err
	}
	script, render, balance, known := s.autopilotPrices(ctx, tenantID, cfg)
	return BuildDryRun(cfg, b, script, render, balance, known), nil
}

// autopilotPrices reads the script price and the dearest allowed tier's render price.
func (s *Service) autopilotPrices(ctx context.Context, tenantID uuid.UUID, cfg AutopilotConfig) (script, render, balance int, known bool) {
	if s.forecaster == nil {
		return 0, 0, -1, false
	}
	fc, err := s.forecaster(ctx, tenantID, OutputScript)
	if err != nil {
		s.logger.Warn("autopilot dry run: prices unavailable", zap.Error(err))
		return 0, 0, -1, false
	}
	script, balance = fc.CreditsNeeded, fc.Balance
	for _, tier := range cfg.VideoTiers {
		fc, err := s.forecaster(ctx, tenantID, tierOutput[tier])
		if err != nil {
			s.logger.Warn("autopilot dry run: prices unavailable", zap.String("tier", tier), zap.Error(err))
			return 0, 0, -1, false
		}
		render = max(render, fc.CreditsNeeded)
	}
	return script, render, balance, true
}

// DryRunAutopilot handles GET /api/v1/agents/custom/{id}/autopilot/dry-run.
// @Summary Preview an ad autopilot's next run and monthly credits
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Agent UUID"
// @Success 200 {object} DryRun
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /agents/custom/{id}/autopilot/dry-run [get]
func (h *Handler) DryRunAutopilot(w http.ResponseWriter, r *http.Request) {
	tenantID, id, ok := h.autopilotParams(w, r)
	if !ok {
		return
	}
	d, err := h.service.DryRunAutopilot(r.Context(), tenantID, id)
	switch {
	case err == nil:
		httpapi.JSON(w, http.StatusOK, d)
	case errors.Is(err, ErrInvalid):
		httpapi.Error(w, http.StatusBadRequest, "invalid_autopilot", err.Error(), nil)
	case ent.IsNotFound(err):
		httpapi.Error(w, http.StatusNotFound, "not_found", "agent not found", nil)
	default:
		h.logger.Error("autopilot dry run", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "dry_run_failed", "failed to preview the agent", nil)
	}
}
