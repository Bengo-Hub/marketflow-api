package analytics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/nurturedeliveryevent"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/profileevent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CampaignMetrics handles GET /api/v1/analytics/campaigns/{id}.
// Returns a daily time series of ad-platform metrics (impressions, clicks,
// spend, leads, CTR, CPL) for the last `days` days (default 30, max 90).
// @Summary Per-campaign ad metrics time series
// @Description Daily rollups from `ad_metrics`. Populated by the 15-minute insights worker for Meta campaigns — returns an empty series for manual or newly-connected campaigns.
// @Tags Analytics
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Param days query int false "Lookback window (1-90, default 30)"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /analytics/campaigns/{id} [get]
func (h *Handler) CampaignMetrics(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenant(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing X-Tenant-ID", nil)
		return
	}
	campaignID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "campaign id must be a UUID", nil)
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := h.db.AdMetrics.Query().
		Where(
			admetrics.TenantIDEQ(tenantID),
			admetrics.CampaignIDEQ(campaignID),
			admetrics.DateGTE(cutoff),
		).
		All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	series := make([]map[string]any, 0, len(rows))
	var totalImpr, totalClicks, totalLeads int64
	var totalSpend float64
	for _, m := range rows {
		series = append(series, map[string]any{
			"date":        m.Date.Format("2006-01-02"),
			"impressions": m.Impressions,
			"clicks":      m.Clicks,
			"spend":       m.Spend,
			"leads":       m.Leads,
			"ctr":         m.Ctr,
			"cpl":         m.Cpl,
			"currency":    m.Currency,
		})
		totalImpr += m.Impressions
		totalClicks += m.Clicks
		totalLeads += m.Leads
		totalSpend += m.Spend
	}
	avgCTR := 0.0
	if totalImpr > 0 {
		avgCTR = float64(totalClicks) / float64(totalImpr) * 100
	}
	avgCPL := 0.0
	if totalLeads > 0 {
		avgCPL = totalSpend / float64(totalLeads)
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"campaign_id": campaignID,
		"days":        days,
		"series":      series,
		"totals": map[string]any{
			"impressions":   totalImpr,
			"clicks":        totalClicks,
			"leads":         totalLeads,
			"spend":         totalSpend,
			"avg_ctr":       avgCTR,
			"avg_cpl":       avgCPL,
			"data_points":   len(series),
		},
	})
}

// SequenceMetrics handles GET /api/v1/analytics/sequences/{id}.
// Aggregates NurtureDeliveryEvent counts by event_type for a single sequence
// and bins them by day. Gives us open/click/reply rates per nurture sequence.
// @Summary Per-sequence nurture analytics
// @Tags Analytics
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Sequence UUID"
// @Param days query int false "Lookback window (default 30)"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /analytics/sequences/{id} [get]
func (h *Handler) SequenceMetrics(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenant(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing X-Tenant-ID", nil)
		return
	}
	sequenceID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "sequence id must be a UUID", nil)
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	// Pull every enrollment for this sequence so we can correlate delivery
	// events back without going through the enrollment FK in every query.
	enrollmentIDs := h.db.NurtureEnrollment.Query().
		Where(
			nurtureenrollment.TenantIDEQ(tenantID),
			nurtureenrollment.SequenceIDEQ(sequenceID),
		).IDsX(r.Context())

	enrolled, _ := h.db.NurtureEnrollment.Query().
		Where(
			nurtureenrollment.TenantIDEQ(tenantID),
			nurtureenrollment.SequenceIDEQ(sequenceID),
		).Count(r.Context())
	completed, _ := h.db.NurtureEnrollment.Query().
		Where(
			nurtureenrollment.TenantIDEQ(tenantID),
			nurtureenrollment.SequenceIDEQ(sequenceID),
			nurtureenrollment.StatusEQ("completed"),
		).Count(r.Context())

	counts := map[string]int{"sent": 0, "delivered": 0, "opened": 0, "clicked": 0, "bounced": 0, "replied": 0, "unsubscribed": 0, "failed": 0}
	if len(enrollmentIDs) > 0 {
		rows, err := h.db.NurtureDeliveryEvent.Query().
			Where(
				nurturedeliveryevent.TenantIDEQ(tenantID),
				nurturedeliveryevent.EnrollmentIDIn(enrollmentIDs...),
				nurturedeliveryevent.CreatedAtGTE(cutoff),
			).
			All(r.Context())
		if err == nil {
			for _, ev := range rows {
				counts[ev.EventType]++
			}
		}
	}
	rate := func(num, den int) float64 {
		if den == 0 {
			return 0
		}
		return float64(num) / float64(den) * 100
	}
	sent := counts["sent"]
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"sequence_id": sequenceID,
		"days":        days,
		"enrollments": map[string]any{
			"total":     enrolled,
			"completed": completed,
			"active":    enrolled - completed,
		},
		"counts": counts,
		"rates": map[string]any{
			"delivery_rate": rate(counts["delivered"], sent),
			"open_rate":     rate(counts["opened"], sent),
			"click_rate":    rate(counts["clicked"], sent),
			"reply_rate":    rate(counts["replied"], sent),
			"bounce_rate":   rate(counts["bounced"], sent),
		},
	})
}

// ProfileMetrics handles GET /api/v1/analytics/profile.
// Aggregates ProfileEvent counts for the tenant's public /p/{slug} page.
// @Summary Public profile visitor analytics
// @Tags Analytics
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param days query int false "Lookback window (default 30)"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /analytics/profile [get]
func (h *Handler) ProfileMetrics(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenant(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing X-Tenant-ID", nil)
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	rows, err := h.db.ProfileEvent.Query().
		Where(
			profileevent.TenantIDEQ(tenantID),
			profileevent.CreatedAtGTE(cutoff),
		).
		All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	counts := map[string]int{}
	uniqueVisitors := map[string]struct{}{}
	for _, ev := range rows {
		counts[ev.EventType]++
		if ev.IPHash != "" {
			uniqueVisitors[ev.IPHash] = struct{}{}
		}
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"days":              days,
		"counts":            counts,
		"unique_visitors":   len(uniqueVisitors),
		"total_events":      len(rows),
	})
}
