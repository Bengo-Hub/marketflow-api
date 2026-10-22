package marketingos

import (
	"context"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/google/uuid"
)

// BriefMetric is one daily-brief number: yesterday's value and the daily average
// of the seven days before it. marketflow-ai only words the change.
type BriefMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Baseline float64 `json:"baseline"`
	Unit     string  `json:"unit,omitempty"`
}

// BriefMetrics computes the brief for the tenant-local day before `day`.
// Customers come first: orders, deals won, leads; then the cost side.
// Every query is tenant-scoped, bounded to eight days and uses an index:
// leads(tenant_id, created_at), ad_metrics(tenant_id, date), deals(tenant_id,
// status), scheduled_meetings(tenant_id, scheduled_at) and activities(tenant_id,
// activity_type). Each query carries one aggregate (no unaliased SUM pairs).
func (s *Service) BriefMetrics(ctx context.Context, tenantID uuid.UUID, dayStart time.Time) ([]BriefMetric, error) {
	from := dayStart.AddDate(0, 0, -1) // yesterday 00:00 local
	baseFrom := from.AddDate(0, 0, -7) // the seven days before yesterday

	type window struct{ a, b time.Time }
	yday, base := window{from, dayStart}, window{baseFrom, from}

	countLeads := func(w window, adOnly bool) (int, error) {
		q := s.db.Lead.Query().Where(lead.TenantID(tenantID), lead.CreatedAtGTE(w.a), lead.CreatedAtLT(w.b))
		if adOnly {
			q = q.Where(lead.CampaignIDNotNil())
		}
		return q.Count(ctx)
	}
	countOrders := func(w window) (int, error) {
		return s.db.Activity.Query().Where(activity.TenantID(tenantID), activity.ActivityType("order"),
			activity.PerformedAtGTE(w.a), activity.PerformedAtLT(w.b)).Count(ctx)
	}
	countWon := func(w window) (int, error) {
		return s.db.Deal.Query().Where(deal.TenantID(tenantID), deal.Status("won"),
			deal.WonAtGTE(w.a), deal.WonAtLT(w.b)).Count(ctx)
	}
	sumWon := func(w window) (float64, error) {
		return sumFloat(ctx, s.db.Deal.Query().Where(deal.TenantID(tenantID), deal.Status("won"),
			deal.WonAtGTE(w.a), deal.WonAtLT(w.b)).Aggregate(coalesceSum(deal.FieldValue)))
	}
	countMeetings := func(w window) (int, error) {
		return s.db.ScheduledMeeting.Query().Where(scheduledmeeting.TenantID(tenantID),
			scheduledmeeting.ScheduledAtGTE(w.a), scheduledmeeting.ScheduledAtLT(w.b)).Count(ctx)
	}
	adWhere := func(w window) *ent.AdMetricsQuery {
		return s.db.AdMetrics.Query().Where(admetrics.TenantID(tenantID), admetrics.DateGTE(w.a), admetrics.DateLT(w.b))
	}
	sumSpend := func(w window) (float64, error) {
		return sumFloat(ctx, adWhere(w).Aggregate(coalesceSum(admetrics.FieldSpend)))
	}
	sumAdLeads := func(w window) (float64, error) {
		return sumFloat(ctx, adWhere(w).Aggregate(coalesceSum(admetrics.FieldLeads)))
	}

	var out []BriefMetric
	addCount := func(key, label string, f func(window) (int, error)) error {
		y, err := f(yday)
		if err != nil {
			return err
		}
		b, err := f(base)
		if err != nil {
			return err
		}
		out = append(out, BriefMetric{Key: key, Label: label, Value: float64(y), Baseline: float64(b) / 7})
		return nil
	}
	addSum := func(key, label, unit string, f func(window) (float64, error)) (float64, float64, error) {
		y, err := f(yday)
		if err != nil {
			return 0, 0, err
		}
		b, err := f(base)
		if err != nil {
			return 0, 0, err
		}
		out = append(out, BriefMetric{Key: key, Label: label, Value: y, Baseline: b / 7, Unit: unit})
		return y, b / 7, nil
	}

	if err := addCount("orders", "Orders from CRM contacts", countOrders); err != nil {
		return nil, err
	}
	if err := addCount("deals_won", "Deals won", countWon); err != nil {
		return nil, err
	}
	if _, _, err := addSum("deal_value", "Value of deals won", "KES", sumWon); err != nil {
		return nil, err
	}
	if err := addCount("leads", "New leads", func(w window) (int, error) { return countLeads(w, false) }); err != nil {
		return nil, err
	}
	if err := addCount("ad_leads", "Leads from ads", func(w window) (int, error) { return countLeads(w, true) }); err != nil {
		return nil, err
	}
	if err := addCount("meetings", "Meetings booked", countMeetings); err != nil {
		return nil, err
	}
	spendY, spendB, err := addSum("ad_spend", "Ad spend", "KES", sumSpend)
	if err != nil {
		return nil, err
	}
	adLeadsY, err := sumAdLeads(yday)
	if err != nil {
		return nil, err
	}
	adLeadsB, err := sumAdLeads(base)
	if err != nil {
		return nil, err
	}
	// Cost per lead is computed here, never by a model; only when both days have leads.
	if adLeadsY > 0 && adLeadsB > 0 {
		out = append(out, BriefMetric{Key: "cpl", Label: "Cost per ad lead", Value: spendY / adLeadsY, Baseline: spendB / (adLeadsB / 7), Unit: "KES"})
	}
	return out, nil
}

// HasActivity reports whether a brief is worth sending: anything moved in the
// eight days it covers.
func HasActivity(metrics []BriefMetric) bool {
	for _, m := range metrics {
		if m.Value != 0 || m.Baseline != 0 {
			return true
		}
	}
	return false
}

// coalesceSum is SUM(field) as "v", zero when no rows match, so the scan never
// meets a NULL and the column always has its own alias.
func coalesceSum(field string) ent.AggregateFunc {
	return func(s *sql.Selector) string {
		return sql.As("COALESCE(SUM("+s.C(field)+"), 0)", "v")
	}
}

func sumFloat(ctx context.Context, sel interface {
	Scan(context.Context, any) error
}) (float64, error) {
	var rows []struct {
		V float64 `json:"v"`
	}
	if err := sel.Scan(ctx, &rows); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].V, nil
}
