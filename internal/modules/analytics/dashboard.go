package analytics

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"golang.org/x/sync/errgroup"

	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/chatsession"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// dashboardCacheTTL is short: the numbers feel live, and a busy dashboard costs the
// database one set of queries per tenant per minute instead of one per page view.
const dashboardCacheTTL = 60 * time.Second

// topSources is how many lead sources the dashboard names before grouping the rest.
const topSources = 8

// dashboardTZ buckets the daily series by local business days.
var dashboardTZ = "Africa/Nairobi"

// DashboardData is the /analytics/dashboard payload. The week figures are kept for
// existing clients; trend and sources cover the requested window.
type DashboardData struct {
	Leads        LeadStats      `json:"leads"`
	ChatSessions map[string]int `json:"chat_sessions"`
	Campaigns    map[string]int `json:"campaigns"`
	Meetings     map[string]int `json:"meetings"`
	Nurture      map[string]int `json:"nurture"`
	Days         int            `json:"days"`
	Daily        []DayCount     `json:"daily"`
	BySource     []SourceCount  `json:"by_source"`
	GeneratedAt  string         `json:"generated_at"`
}

// LeadStats are the lead headline figures.
type LeadStats struct {
	Total          int     `json:"total"`
	ThisWeek       int     `json:"this_week"`
	PrevWeek       int     `json:"prev_week"`
	DeltaPercent   float64 `json:"delta_percent"`
	Converted      int     `json:"converted"`
	ConversionRate float64 `json:"conversion_rate"`
	InWindow       int     `json:"in_window"`
}

// DayCount is one point of the daily lead series.
type DayCount struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

// SourceCount is the number of leads from one source in the window.
type SourceCount struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
}

// Dashboard returns the tenant dashboard aggregates.
// @Summary Tenant dashboard aggregates
// @Description Lead headline figures with week-over-week change, a daily lead series and leads by source over the window, plus chat, campaign, meeting and nurture counts.
// @Tags Analytics
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param days query int false "Window for the series and sources: 7, 30 or 90 (default 30)"
// @Success 200 {object} DashboardData
// @Security bearerAuth
// @Router /analytics/dashboard [get]
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenant(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing X-Tenant-ID", nil)
		return
	}
	days := dashboardDays(r.URL.Query().Get("days"))
	key := sharedcache.Key("mf", "analytics", "dashboard", tenantID.String(), strconv.Itoa(days))
	data, err := sharedcache.GetOrSet(r.Context(), h.cache, key, dashboardCacheTTL, func(ctx context.Context) (DashboardData, error) {
		return h.dashboard(ctx, tenantID, days, time.Now().UTC())
	})
	if err != nil {
		h.logger.Error("dashboard query failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "could not load the dashboard", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, data)
}

func dashboardDays(v string) int {
	switch v {
	case "7":
		return 7
	case "90":
		return 90
	default:
		return 30
	}
}

// dashboard runs the aggregate queries, at most four at a time so one page view
// never holds more than four pooled connections.
func (h *Handler) dashboard(ctx context.Context, tenantID uuid.UUID, days int, now time.Time) (DashboardData, error) {
	weekAgo := now.Add(-7 * 24 * time.Hour)
	prevWeek := now.Add(-14 * 24 * time.Hour)
	windowStart := now.Add(-time.Duration(days) * 24 * time.Hour)

	var (
		leads                                         LeadStats
		daily                                         []DayCount
		sources                                       []SourceCount
		chats, campaignsActive, meetings, enrollments int
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)

	// One scan over (tenant_id, created_at) for every lead figure, instead of one
	// COUNT per figure.
	g.Go(func() error {
		var rows []struct {
			Total     int `json:"total"`
			ThisWeek  int `json:"this_week"`
			PrevWeek  int `json:"prev_week"`
			Converted int `json:"converted"`
			InWindow  int `json:"in_window"`
		}
		err := h.db.Lead.Query().Where(lead.TenantIDEQ(tenantID)).Modify(func(s *entsql.Selector) {
			c := s.C(lead.FieldCreatedAt)
			s.Select().
				AppendSelectExprAs(entsql.Expr("COUNT(*)"), "total").
				AppendSelectExprAs(countSince(c, weekAgo), "this_week").
				AppendSelectExprAs(entsql.ExprFunc(func(b *entsql.Builder) {
					b.WriteString("COUNT(*) FILTER (WHERE " + c + " >= ").Arg(prevWeek).
						WriteString(" AND " + c + " < ").Arg(weekAgo).WriteString(")")
				}), "prev_week").
				AppendSelectExprAs(entsql.ExprFunc(func(b *entsql.Builder) {
					b.WriteString("COUNT(*) FILTER (WHERE " + s.C(lead.FieldStatus) + " = ").Arg("converted").WriteString(")")
				}), "converted").
				AppendSelectExprAs(countSince(c, windowStart), "in_window")
		}).Scan(gctx, &rows)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			r := rows[0]
			leads = LeadStats{Total: r.Total, ThisWeek: r.ThisWeek, PrevWeek: r.PrevWeek, Converted: r.Converted, InWindow: r.InWindow}
		}
		return nil
	})

	g.Go(func() error {
		var rows []DayCount
		err := h.db.Lead.Query().Where(lead.TenantIDEQ(tenantID), lead.CreatedAtGTE(windowStart)).Modify(func(s *entsql.Selector) {
			day := "to_char(date_trunc('day', " + s.C(lead.FieldCreatedAt) + " AT TIME ZONE '" + dashboardTZ + "'), 'YYYY-MM-DD')"
			s.Select().AppendSelectExprAs(entsql.Expr(day), "day").
				AppendSelectExprAs(entsql.Expr("COUNT(*)"), "count").
				GroupBy(day)
		}).Scan(gctx, &rows)
		if err != nil {
			return err
		}
		daily = fillDays(rows, now, days)
		return nil
	})

	g.Go(func() error {
		var rows []SourceCount
		err := h.db.Lead.Query().Where(lead.TenantIDEQ(tenantID), lead.CreatedAtGTE(windowStart)).Modify(func(s *entsql.Selector) {
			src := "COALESCE(NULLIF(" + s.C(lead.FieldSource) + ", ''), 'unknown')"
			s.Select().AppendSelectExprAs(entsql.Expr(src), "source").
				AppendSelectExprAs(entsql.Expr("COUNT(*)"), "count").
				GroupBy(src)
		}).Scan(gctx, &rows)
		if err != nil {
			return err
		}
		sources = topN(rows, topSources)
		return nil
	})

	g.Go(func() (err error) {
		chats, err = h.db.ChatSession.Query().
			Where(chatsession.TenantIDEQ(tenantID), chatsession.StartedAtGTE(weekAgo)).Count(gctx)
		return err
	})
	g.Go(func() (err error) {
		campaignsActive, err = h.db.Campaign.Query().
			Where(campaign.TenantIDEQ(tenantID), campaign.StatusEQ("active")).Count(gctx)
		return err
	})
	g.Go(func() (err error) {
		meetings, err = h.db.ScheduledMeeting.Query().Where(
			scheduledmeeting.TenantIDEQ(tenantID),
			scheduledmeeting.StatusEQ("scheduled"),
			scheduledmeeting.ScheduledAtGTE(now),
			scheduledmeeting.ScheduledAtLT(now.Add(7*24*time.Hour)),
		).Count(gctx)
		return err
	})
	g.Go(func() (err error) {
		enrollments, err = h.db.NurtureEnrollment.Query().
			Where(nurtureenrollment.TenantIDEQ(tenantID), nurtureenrollment.StatusEQ("active")).Count(gctx)
		return err
	})

	if err := g.Wait(); err != nil {
		return DashboardData{}, err
	}

	if leads.Total > 0 {
		leads.ConversionRate = float64(leads.Converted) / float64(leads.Total) * 100
	}
	if leads.PrevWeek > 0 {
		leads.DeltaPercent = (float64(leads.ThisWeek) - float64(leads.PrevWeek)) / float64(leads.PrevWeek) * 100
	}
	return DashboardData{
		Leads:        leads,
		ChatSessions: map[string]int{"this_week": chats},
		Campaigns:    map[string]int{"active": campaignsActive},
		Meetings:     map[string]int{"upcoming_week": meetings},
		Nurture:      map[string]int{"active_enrollments": enrollments},
		Days:         days,
		Daily:        daily,
		BySource:     sources,
		GeneratedAt:  now.Format(time.RFC3339),
	}, nil
}

// countSince is COUNT(*) FILTER (WHERE col >= t), with t bound as a parameter.
func countSince(col string, t time.Time) entsql.Querier {
	return entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("COUNT(*) FILTER (WHERE " + col + " >= ").Arg(t).WriteString(")")
	})
}

// fillDays returns one point per local day in the window, oldest first, with zero
// for days that had no leads.
func fillDays(rows []DayCount, now time.Time, days int) []DayCount {
	loc, err := time.LoadLocation(dashboardTZ)
	if err != nil {
		loc = time.UTC
	}
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.Day] = r.Count
	}
	today := now.In(loc)
	out := make([]DayCount, 0, days)
	for i := days - 1; i >= 0; i-- {
		d := today.AddDate(0, 0, -i).Format("2006-01-02")
		out = append(out, DayCount{Day: d, Count: counts[d]})
	}
	return out
}

// topN keeps the n largest sources and folds the rest into "other".
func topN(rows []SourceCount, n int) []SourceCount {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Source < rows[j].Source
	})
	if len(rows) == 0 {
		// An empty array, never null: clients read .length on it.
		return []SourceCount{}
	}
	if len(rows) <= n {
		return rows
	}
	other := 0
	for _, r := range rows[n:] {
		other += r.Count
	}
	return append(rows[:n:n], SourceCount{Source: "other", Count: other})
}
