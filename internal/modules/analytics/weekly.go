package analytics

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

// Weekly report (Sprint 7 #52): the figures of the Monday email every business gets, with or
// without an ad autopilot. The autopilot's own email (worker.AutopilotWeekly) covers one agent's
// ads; this one covers the whole business.

// topWeeklyCampaigns is how many campaigns the email names.
const topWeeklyCampaigns = 3

// WeekDigest is one business's last seven days.
type WeekDigest struct {
	From, To     time.Time
	Leads        int
	LeadsBefore  int // the seven days before
	Converted    int
	DealsWon     int
	WonValue     float64
	Spend        float64
	Currency     string
	CostPerLead  *float64
	TopCampaigns []CampaignROI
}

// Empty reports a week with nothing to tell: no leads, no sales and no ad spend.
func (d WeekDigest) Empty() bool {
	return d.Leads == 0 && d.DealsWon == 0 && d.Converted == 0 && d.Spend == 0
}

// WeeklyDigest computes the seven days before now for one tenant.
func WeeklyDigest(ctx context.Context, db *ent.Client, tenantID uuid.UUID, now time.Time) (WeekDigest, error) {
	d := WeekDigest{From: now.AddDate(0, 0, -7), To: now}
	before := now.AddDate(0, 0, -14)
	var roi ROIReport
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(3)
	g.Go(func() (err error) {
		d.Leads, err = db.Lead.Query().Where(lead.TenantIDEQ(tenantID), lead.CreatedAtGTE(d.From), lead.CreatedAtLT(now)).Count(gctx)
		return err
	})
	g.Go(func() (err error) {
		d.LeadsBefore, err = db.Lead.Query().Where(lead.TenantIDEQ(tenantID), lead.CreatedAtGTE(before), lead.CreatedAtLT(d.From)).Count(gctx)
		return err
	})
	g.Go(func() (err error) {
		d.Converted, err = db.Lead.Query().Where(lead.TenantIDEQ(tenantID), lead.ConvertedAtGTE(d.From), lead.ConvertedAtLT(now)).Count(gctx)
		return err
	})
	g.Go(func() error {
		won, err := db.Deal.Query().
			Where(deal.TenantIDEQ(tenantID), deal.StatusEQ("won"), deal.WonAtGTE(d.From), deal.WonAtLT(now)).
			Select(deal.FieldValue).All(gctx)
		if err != nil {
			return err
		}
		d.DealsWon = len(won)
		for _, w := range won {
			if w.Value != nil {
				d.WonValue += *w.Value
			}
		}
		return nil
	})
	g.Go(func() (err error) {
		roi, err = ComputeROI(gctx, db, tenantID, d.From, now)
		return err
	})
	if err := g.Wait(); err != nil {
		return d, err
	}
	d.Spend, d.Currency, d.CostPerLead = roi.Totals.Spend, roi.Currency, roi.Totals.CostPerLead
	for _, c := range roi.Campaigns {
		if c.Leads == 0 && c.Spend == 0 && c.WonDeals == 0 {
			continue
		}
		d.TopCampaigns = append(d.TopCampaigns, c)
	}
	// Most leads first for the email; the ROI order puts revenue first.
	sortByLeads(d.TopCampaigns)
	if len(d.TopCampaigns) > topWeeklyCampaigns {
		d.TopCampaigns = d.TopCampaigns[:topWeeklyCampaigns]
	}
	return d, nil
}

func sortByLeads(cs []CampaignROI) {
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Leads > cs[j].Leads })
}
