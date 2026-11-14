package analytics

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	tenantmod "github.com/bengobox/marketflow-api/internal/modules/tenant"
)

// Campaign return (Sprint 7 #54). Cost per lead says what a lead cost; this says what the leads
// brought back. Revenue is the value of won deals whose lead came from the campaign. A converted
// lead with no won deal yet counts at the average sale value: the owner's own figure when set
// (Settings, Reports), else the average of the tenant's won deals over averageSaleDays. Leads are
// counted by when they arrived, so a campaign keeps the deals its leads closed later.

// averageSaleDays is how far back won deals set the average sale value.
const averageSaleDays = 180

// maxROICampaigns bounds the campaign list; the totals still cover every campaign.
const maxROICampaigns = 50

// CampaignROI is one campaign's spend against what its leads brought back.
type CampaignROI struct {
	CampaignID       uuid.UUID `json:"campaign_id"`
	Name             string    `json:"name"`
	Platform         string    `json:"platform,omitempty"`
	Currency         string    `json:"currency"`
	Spend            float64   `json:"spend"`
	Leads            int       `json:"leads"`
	Converted        int       `json:"converted"`
	WonDeals         int       `json:"won_deals"`
	Revenue          float64   `json:"revenue"`
	EstimatedRevenue float64   `json:"estimated_revenue"`
	CostPerLead      *float64  `json:"cost_per_lead,omitempty"`
	ROIPercent       *float64  `json:"roi_percent,omitempty"`
	ROAS             *float64  `json:"roas,omitempty"`
}

// ROITotals add up every campaign in the window.
type ROITotals struct {
	Spend            float64  `json:"spend"`
	Leads            int      `json:"leads"`
	Converted        int      `json:"converted"`
	WonDeals         int      `json:"won_deals"`
	Revenue          float64  `json:"revenue"`
	EstimatedRevenue float64  `json:"estimated_revenue"`
	CostPerLead      *float64 `json:"cost_per_lead,omitempty"`
	ROIPercent       *float64 `json:"roi_percent,omitempty"`
	ROAS             *float64 `json:"roas,omitempty"`
}

// ROIReport is the /analytics/roi payload.
type ROIReport struct {
	Days             int           `json:"days"`
	Currency         string        `json:"currency"`
	AverageSaleValue float64       `json:"average_sale_value"`
	AverageFrom      string        `json:"average_from"` // setting | won_deals | none
	Campaigns        []CampaignROI `json:"campaigns"`
	Totals           ROITotals     `json:"totals"`
	GeneratedAt      string        `json:"generated_at"`
}

// ROI handles GET /api/v1/analytics/roi.
// @Summary Campaign return on spend
// @Description Per campaign: ad spend, leads, converted leads, won deals and their value, estimated revenue (converted leads without a won deal at the average sale value) and the return on spend. Leads are counted by arrival in the window.
// @Tags Analytics
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param days query int false "Window: 7, 30 or 90 (default 30)"
// @Success 200 {object} ROIReport
// @Security bearerAuth
// @Router /analytics/roi [get]
func (h *Handler) ROI(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenant(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing X-Tenant-ID", nil)
		return
	}
	days := dashboardDays(r.URL.Query().Get("days"))
	key := sharedcache.Key("mf", "analytics", "roi", tenantID.String(), strconv.Itoa(days))
	data, err := sharedcache.GetOrSet(r.Context(), h.cache, key, dashboardCacheTTL, func(ctx context.Context) (ROIReport, error) {
		now := time.Now().UTC()
		return ComputeROI(ctx, h.db, tenantID, now.AddDate(0, 0, -days), now)
	})
	if err != nil {
		h.logger.Error("roi query failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "could not work out the campaign return", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, data)
}

// ComputeROI works out every campaign's return for leads that arrived in [since, until). The
// weekly report email and the PDF report use it too.
func ComputeROI(ctx context.Context, db *ent.Client, tenantID uuid.UUID, since, until time.Time) (ROIReport, error) {
	out := ROIReport{
		Days:        int(until.Sub(since).Hours()/24 + 0.5),
		Campaigns:   []CampaignROI{},
		AverageFrom: "none",
		GeneratedAt: until.UTC().Format(time.RFC3339),
	}
	rows := map[uuid.UUID]*CampaignROI{}
	row := func(id uuid.UUID) *CampaignROI {
		if c, ok := rows[id]; ok {
			return c
		}
		c := &CampaignROI{CampaignID: id}
		rows[id] = c
		return c
	}

	// Spend per campaign from the daily ad metrics.
	var spend []struct {
		CampaignID uuid.UUID `json:"campaign_id"`
		Spend      float64   `json:"spend"`
		Currency   string    `json:"currency"`
	}
	err := db.AdMetrics.Query().
		Where(admetrics.TenantID(tenantID), admetrics.DateGTE(since), admetrics.DateLT(until)).
		Modify(func(s *entsql.Selector) {
			c := s.C(admetrics.FieldCampaignID)
			s.Select(c).
				AppendSelectExprAs(entsql.Expr("COALESCE(SUM("+s.C(admetrics.FieldSpend)+"), 0)"), "spend").
				AppendSelectExprAs(entsql.Expr("COALESCE(MAX("+s.C(admetrics.FieldCurrency)+"), '')"), "currency").
				GroupBy(c)
		}).Scan(ctx, &spend)
	if err != nil {
		return out, err
	}
	for _, s := range spend {
		c := row(s.CampaignID)
		c.Spend, c.Currency = s.Spend, s.Currency
	}

	// Leads and converted leads per campaign.
	var leads []struct {
		CampaignID uuid.UUID `json:"campaign_id"`
		Leads      int       `json:"leads"`
		Converted  int       `json:"converted"`
	}
	err = db.Lead.Query().
		Where(lead.TenantIDEQ(tenantID), lead.CampaignIDNotNil(), lead.CreatedAtGTE(since), lead.CreatedAtLT(until)).
		Modify(func(s *entsql.Selector) {
			c := s.C(lead.FieldCampaignID)
			s.Select(c).
				AppendSelectExprAs(entsql.Expr("COUNT(*)"), "leads").
				AppendSelectExprAs(entsql.ExprFunc(func(b *entsql.Builder) {
					b.WriteString("COUNT(*) FILTER (WHERE " + s.C(lead.FieldStatus) + " = ").Arg("converted").WriteString(")")
				}), "converted").
				GroupBy(c)
		}).Scan(ctx, &leads)
	if err != nil {
		return out, err
	}
	for _, l := range leads {
		c := row(l.CampaignID)
		c.Leads, c.Converted = l.Leads, l.Converted
	}

	// Won deals whose lead came from a campaign in the window.
	var won []struct {
		CampaignID uuid.UUID `json:"campaign_id"`
		WonDeals   int       `json:"won_deals"`
		WonLeads   int       `json:"won_leads"`
		Revenue    float64   `json:"revenue"`
	}
	err = db.Deal.Query().
		Where(deal.TenantIDEQ(tenantID), deal.StatusEQ("won"), deal.LeadIDNotNil()).
		Modify(func(s *entsql.Selector) {
			l := entsql.Table(lead.Table)
			s.Join(l).On(s.C(deal.FieldLeadID), l.C(lead.FieldID))
			s.Where(entsql.And(
				entsql.EQ(l.C(lead.FieldTenantID), tenantID),
				entsql.NotNull(l.C(lead.FieldCampaignID)),
				entsql.GTE(l.C(lead.FieldCreatedAt), since),
				entsql.LT(l.C(lead.FieldCreatedAt), until),
			))
			c := l.C(lead.FieldCampaignID)
			s.Select(entsql.As(c, "campaign_id")).
				AppendSelectExprAs(entsql.Expr("COUNT(*)"), "won_deals").
				AppendSelectExprAs(entsql.Expr("COUNT(DISTINCT "+s.C(deal.FieldLeadID)+")"), "won_leads").
				AppendSelectExprAs(entsql.Expr("COALESCE(SUM("+s.C(deal.FieldValue)+"), 0)"), "revenue").
				GroupBy(c)
		}).Scan(ctx, &won)
	if err != nil {
		return out, err
	}
	wonLeads := map[uuid.UUID]int{}
	for _, d := range won {
		c := row(d.CampaignID)
		c.WonDeals, c.Revenue = d.WonDeals, d.Revenue
		wonLeads[d.CampaignID] = d.WonLeads
	}

	avg, from, err := averageSaleValue(ctx, db, tenantID, until)
	if err != nil {
		return out, err
	}
	out.AverageSaleValue, out.AverageFrom = avg, from

	if len(rows) > 0 {
		ids := make([]uuid.UUID, 0, len(rows))
		for id := range rows {
			ids = append(ids, id)
		}
		camps, err := db.Campaign.Query().
			Where(campaign.TenantIDEQ(tenantID), campaign.IDIn(ids...)).
			Select(campaign.FieldID, campaign.FieldName, campaign.FieldPlatform, campaign.FieldCurrency).
			All(ctx)
		if err != nil {
			return out, err
		}
		for _, c := range camps {
			r := rows[c.ID]
			r.Name, r.Platform = c.Name, c.Platform
			if r.Currency == "" {
				r.Currency = c.Currency
			}
		}
	}

	for id, c := range rows {
		// A campaign id from another tenant or a deleted campaign has no name: leave it out.
		if c.Name == "" {
			continue
		}
		if c.Currency == "" {
			c.Currency = "KES"
		}
		pending := c.Converted - wonLeads[id]
		if pending < 0 {
			pending = 0
		}
		c.EstimatedRevenue = c.Revenue + float64(pending)*avg
		c.CostPerLead, c.ROIPercent, c.ROAS = ratios(c.Spend, c.Leads, c.EstimatedRevenue)
		out.Campaigns = append(out.Campaigns, *c)
		out.Totals.Spend += c.Spend
		out.Totals.Leads += c.Leads
		out.Totals.Converted += c.Converted
		out.Totals.WonDeals += c.WonDeals
		out.Totals.Revenue += c.Revenue
		out.Totals.EstimatedRevenue += c.EstimatedRevenue
	}
	out.Totals.CostPerLead, out.Totals.ROIPercent, out.Totals.ROAS = ratios(out.Totals.Spend, out.Totals.Leads, out.Totals.EstimatedRevenue)
	out.Currency = reportCurrency(out.Campaigns)

	sort.Slice(out.Campaigns, func(i, j int) bool {
		a, b := out.Campaigns[i], out.Campaigns[j]
		if a.EstimatedRevenue != b.EstimatedRevenue {
			return a.EstimatedRevenue > b.EstimatedRevenue
		}
		if a.Leads != b.Leads {
			return a.Leads > b.Leads
		}
		if a.Spend != b.Spend {
			return a.Spend > b.Spend
		}
		return a.Name < b.Name
	})
	if len(out.Campaigns) > maxROICampaigns {
		out.Campaigns = out.Campaigns[:maxROICampaigns]
	}
	return out, nil
}

// averageSaleValue is the owner's figure when set, else the mean value of won deals closed in
// the averageSaleDays before until.
func averageSaleValue(ctx context.Context, db *ent.Client, tenantID uuid.UUID, until time.Time) (float64, string, error) {
	t, err := db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return 0, "none", err
	}
	if v := tenantmod.AverageSaleValue(t.Metadata); v > 0 {
		return v, "setting", nil
	}
	var rows []struct {
		Avg float64 `json:"avg"`
		N   int     `json:"n"`
	}
	err = db.Deal.Query().
		Where(deal.TenantIDEQ(tenantID), deal.StatusEQ("won"), deal.ValueGT(0),
			deal.WonAtGTE(until.AddDate(0, 0, -averageSaleDays)), deal.WonAtLT(until)).
		Modify(func(s *entsql.Selector) {
			s.Select().
				AppendSelectExprAs(entsql.Expr("COALESCE(AVG("+s.C(deal.FieldValue)+"), 0)"), "avg").
				AppendSelectExprAs(entsql.Expr("COUNT(*)"), "n")
		}).Scan(ctx, &rows)
	if err != nil {
		return 0, "none", err
	}
	if len(rows) == 0 || rows[0].N == 0 {
		return 0, "none", nil
	}
	return rows[0].Avg, "won_deals", nil
}

// ratios are cost per lead, return on spend in percent and revenue per unit of spend. Each is
// nil when its divisor is zero.
func ratios(spend float64, leads int, revenue float64) (cpl, roi, roas *float64) {
	if leads > 0 && spend > 0 {
		v := spend / float64(leads)
		cpl = &v
	}
	if spend > 0 {
		r := (revenue - spend) / spend * 100
		x := revenue / spend
		roi, roas = &r, &x
	}
	return cpl, roi, roas
}

// reportCurrency is the campaigns' common currency, KES when they disagree or there are none.
func reportCurrency(cs []CampaignROI) string {
	cur := ""
	for _, c := range cs {
		code := strings.ToUpper(strings.TrimSpace(c.Currency))
		if code == "" {
			continue
		}
		if cur == "" {
			cur = code
		} else if cur != code {
			return "KES"
		}
	}
	if cur == "" {
		return "KES"
	}
	return cur
}
