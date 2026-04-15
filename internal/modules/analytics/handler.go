// Package analytics serves the tenant dashboard aggregate query.
package analytics

import (
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/chatsession"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves tenant-scoped aggregate stats for the dashboard.
type Handler struct {
	db     *ent.Client
	logger *zap.Logger
}

func NewHandler(db *ent.Client, logger *zap.Logger) *Handler {
	return &Handler{db: db, logger: logger.Named("analytics")}
}

// Dashboard returns aggregated counts + deltas for the tenant dashboard.
// @Summary Tenant dashboard aggregates
// @Description Counts + week-over-week deltas for leads, contacts, chat sessions, campaigns, meetings, active nurture enrollments.
// @Tags Analytics
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /analytics/dashboard [get]
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenant(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing X-Tenant-ID", nil)
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	weekAgo := now.Add(-7 * 24 * time.Hour)
	prevWeek := now.Add(-14 * 24 * time.Hour)

	// Leads
	leadTotal, _ := h.db.Lead.Query().Where(lead.TenantIDEQ(tenantID)).Count(ctx)
	leadsThisWeek, _ := h.db.Lead.Query().Where(lead.TenantIDEQ(tenantID), lead.CreatedAtGTE(weekAgo)).Count(ctx)
	leadsPrevWeek, _ := h.db.Lead.Query().Where(lead.TenantIDEQ(tenantID), lead.CreatedAtGTE(prevWeek), lead.CreatedAtLT(weekAgo)).Count(ctx)
	leadsConverted, _ := h.db.Lead.Query().Where(lead.TenantIDEQ(tenantID), lead.StatusEQ("converted")).Count(ctx)

	// Chat sessions (this week)
	sessionsThisWeek, _ := h.db.ChatSession.Query().
		Where(chatsession.TenantIDEQ(tenantID), chatsession.StartedAtGTE(weekAgo)).
		Count(ctx)

	// Campaigns (active)
	campaignsActive, _ := h.db.Campaign.Query().
		Where(campaign.TenantIDEQ(tenantID), campaign.StatusEQ("active")).
		Count(ctx)

	// Meetings (scheduled + next 7 days)
	meetingsUpcoming, _ := h.db.ScheduledMeeting.Query().
		Where(
			scheduledmeeting.TenantIDEQ(tenantID),
			scheduledmeeting.StatusEQ("scheduled"),
			scheduledmeeting.ScheduledAtGTE(now),
			scheduledmeeting.ScheduledAtLT(now.Add(7*24*time.Hour)),
		).
		Count(ctx)

	// Nurture enrollments (active)
	enrollmentsActive, _ := h.db.NurtureEnrollment.Query().
		Where(nurtureenrollment.TenantIDEQ(tenantID), nurtureenrollment.StatusEQ("active")).
		Count(ctx)

	conversionRate := 0.0
	if leadTotal > 0 {
		conversionRate = float64(leadsConverted) / float64(leadTotal) * 100
	}
	leadsDelta := 0.0
	if leadsPrevWeek > 0 {
		leadsDelta = (float64(leadsThisWeek) - float64(leadsPrevWeek)) / float64(leadsPrevWeek) * 100
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"leads": map[string]any{
			"total":            leadTotal,
			"this_week":        leadsThisWeek,
			"delta_percent":    leadsDelta,
			"converted":        leadsConverted,
			"conversion_rate":  conversionRate,
		},
		"chat_sessions": map[string]any{
			"this_week": sessionsThisWeek,
		},
		"campaigns": map[string]any{
			"active": campaignsActive,
		},
		"meetings": map[string]any{
			"upcoming_week": meetingsUpcoming,
		},
		"nurture": map[string]any{
			"active_enrollments": enrollmentsActive,
		},
		"generated_at": now.Format(time.RFC3339),
	})
}

func extractTenant(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(r.Header.Get("X-Tenant-ID"))
}
