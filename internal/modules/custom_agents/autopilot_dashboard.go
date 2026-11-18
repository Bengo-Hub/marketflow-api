package custom_agents

import (
	"context"
	"errors"
	"net/http"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// The ad autopilot's dashboard (Sprint 20): drafts waiting for the owner and the last seven days'
// results. The run history is GET /agents/custom/{id}/runs and the scheduled posts with their veto
// countdown are GET /agents/autopilot/posts; this read adds what neither has.

// Creative version statuses a draft waits for the owner in (creative.StatusScriptReady and
// creative.StatusReady): the script to review, or the finished ad to approve.
const (
	draftScriptReady = "script_ready"
	draftReady       = "ready"
)

// pendingDraftsLimit caps the pending list; an agent rarely has more than a few drafts waiting.
const pendingDraftsLimit = 50

// AutopilotWeek is one agent's last seven days: ads made, credits used, ad spend, leads and cost
// per lead, all from stored rows. The weekly email sends the same numbers.
type AutopilotWeek struct {
	Since   time.Time `json:"since"`
	AdsMade int       `json:"ads_made"`
	Credits int       `json:"credits"`
	Spend   float64   `json:"spend"`
	Leads   int64     `json:"leads"`
	CPL     float64   `json:"cpl"`
}

// PendingDraft is a draft of the agent's waiting for the owner: a script to review (approve-each
// mode) or a finished ad to approve and post. Veto-window posts are not here: the agent carries
// them on by itself, and they are in the scheduled posts list.
type PendingDraft struct {
	VersionID uuid.UUID `json:"version_id"`
	BriefID   uuid.UUID `json:"brief_id"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AutopilotDashboard is the dashboard read's answer.
type AutopilotDashboard struct {
	Pending []PendingDraft `json:"pending"`
	Week    AutopilotWeek  `json:"week"`
}

// agentBriefs queries the tenant's briefs an agent made (metadata.autopilot_agent_id).
func agentBriefs(db *ent.Client, tenantID, agentID uuid.UUID) *ent.CreativeBriefQuery {
	return db.CreativeBrief.Query().
		Where(creativebrief.TenantID(tenantID), func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(s.C(creativebrief.FieldMetadata) + "->>'autopilot_agent_id' = ").Arg(agentID.String())
			}))
		})
}

// AutopilotWeekSummary reads the agent's briefs, the versions made in the last seven days, the
// boosts of the agent's versions and those boosts' daily metrics. Every query is scoped to the
// tenant.
func AutopilotWeekSummary(ctx context.Context, db *ent.Client, tenantID, agentID uuid.UUID, now time.Time) (AutopilotWeek, error) {
	since := now.UTC().AddDate(0, 0, -7)
	out := AutopilotWeek{Since: since}
	briefs, err := agentBriefs(db, tenantID, agentID).IDs(ctx)
	if err != nil || len(briefs) == 0 {
		return out, err
	}
	versions, err := db.CreativeVersion.Query().
		Where(creativeversion.TenantID(tenantID), creativeversion.BriefIDIn(briefs...), creativeversion.CreatedAtGTE(since)).All(ctx)
	if err != nil {
		return out, err
	}
	out.AdsMade = len(versions)
	for _, v := range versions {
		out.Credits += v.Credits
	}
	allVersions, err := db.CreativeVersion.Query().
		Where(creativeversion.TenantID(tenantID), creativeversion.BriefIDIn(briefs...)).IDs(ctx)
	if err != nil {
		return out, err
	}
	campaigns, err := db.Campaign.Query().
		Where(campaign.TenantIDEQ(tenantID), campaign.CreativeVersionIDIn(allVersions...)).IDs(ctx)
	if err != nil || len(campaigns) == 0 {
		return out, err
	}
	metrics, err := db.AdMetrics.Query().
		Where(admetrics.TenantID(tenantID), admetrics.CampaignIDIn(campaigns...), admetrics.DateGTE(since)).All(ctx)
	if err != nil {
		return out, err
	}
	for _, m := range metrics {
		out.Spend += m.Spend
		out.Leads += m.Leads + m.Conversions
	}
	if out.Leads > 0 {
		out.CPL = out.Spend / float64(out.Leads)
	}
	return out, nil
}

// PendingDrafts lists the agent's drafts waiting for the owner, newest first.
func PendingDrafts(ctx context.Context, db *ent.Client, tenantID, agentID uuid.UUID) ([]PendingDraft, error) {
	briefs, err := agentBriefs(db, tenantID, agentID).All(ctx)
	if err != nil {
		return nil, err
	}
	out := []PendingDraft{}
	if len(briefs) == 0 {
		return out, nil
	}
	titles := make(map[uuid.UUID]string, len(briefs))
	ids := make([]uuid.UUID, 0, len(briefs))
	for _, b := range briefs {
		titles[b.ID] = b.Title
		ids = append(ids, b.ID)
	}
	versions, err := db.CreativeVersion.Query().
		Where(creativeversion.TenantID(tenantID), creativeversion.BriefIDIn(ids...),
			creativeversion.StatusIn(draftScriptReady, draftReady)).
		Order(ent.Desc(creativeversion.FieldCreatedAt)).
		Limit(pendingDraftsLimit).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if inVetoWindow(v.Metadata) {
			continue
		}
		out = append(out, PendingDraft{VersionID: v.ID, BriefID: v.BriefID, Title: titles[v.BriefID], Kind: v.Kind,
			Status: v.Status, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt})
	}
	return out, nil
}

// inVetoWindow reports whether a version is a veto-window video the agent is still carrying on
// (metadata.autopilot mode veto_window, state active): it is not waiting for the owner.
func inVetoWindow(meta map[string]any) bool {
	ap, ok := meta["autopilot"].(map[string]any)
	if !ok {
		return false
	}
	mode, _ := ap["mode"].(string)
	state, _ := ap["state"].(string)
	return mode == "veto_window" && state == "active"
}

// errNotAutopilot is an agent id that is not one of the tenant's ad autopilots.
var errNotAutopilot = errors.New("not an ad autopilot")

// AutopilotDashboard reads one of the tenant's ad autopilots' pending drafts and week.
func (s *Service) AutopilotDashboard(ctx context.Context, tenantID, agentID uuid.UUID, now time.Time) (AutopilotDashboard, error) {
	a, err := s.client.CustomAgent.Query().
		Where(customagent.ID(agentID), customagent.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return AutopilotDashboard{}, err
	}
	if kind, _ := a.Metadata["kind"].(string); kind != KindAdAutopilot {
		return AutopilotDashboard{}, errNotAutopilot
	}
	pending, err := PendingDrafts(ctx, s.client, tenantID, agentID)
	if err != nil {
		return AutopilotDashboard{}, err
	}
	week, err := AutopilotWeekSummary(ctx, s.client, tenantID, agentID, now)
	if err != nil {
		return AutopilotDashboard{}, err
	}
	return AutopilotDashboard{Pending: pending, Week: week}, nil
}

// GetAutopilotDashboard handles GET /api/v1/agents/custom/{id}/autopilot/dashboard.
// @Summary An ad autopilot's drafts waiting for the owner and its last seven days
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Agent UUID"
// @Success 200 {object} AutopilotDashboard
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /agents/custom/{id}/autopilot/dashboard [get]
func (h *Handler) GetAutopilotDashboard(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid agent ID", nil)
		return
	}
	d, err := h.service.AutopilotDashboard(r.Context(), tenantID, id, time.Now())
	switch {
	case ent.IsNotFound(err), errors.Is(err, errNotAutopilot):
		httpapi.Error(w, http.StatusNotFound, "not_found", "no ad autopilot with this id", nil)
		return
	case err != nil:
		h.logger.Error("autopilot dashboard read failed", zap.String("agent_id", id.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "read_failed", "could not read the dashboard", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, d)
}
