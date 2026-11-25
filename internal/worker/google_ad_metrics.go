package worker

import (
	"context"
	"net/http"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"go.uber.org/zap"

	sharedcache "github.com/Bengo-Hub/cache"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/modules/googleads"
)

// GoogleAdMetrics writes the daily results of the Google Ads campaigns MarketFlow created (Run
// ads) into the same ad metrics Meta's insights use, so an agent's month to date spend and the
// campaign pages count Google too (owner, 2026-11-25). It only reads from Google.
type GoogleAdMetrics struct {
	db     *ent.Client
	tokens *google.Tokens
	extra  func(ctx context.Context) (map[string]string, error)
	hc     *http.Client
	base   string
	now    func() time.Time
	logger *zap.Logger
}

const (
	googleMetricsEvery = time.Hour
	// googleMetricsBack is how many days are read again each time: Google corrects recent days
	// (late clicks, invalid traffic credits).
	googleMetricsBack = 35
	// googleMetricsAfter is how long after its end a campaign is still read.
	googleMetricsAfter = 7 * 24 * time.Hour
	googleMetricsBatch = 200
)

// NewGoogleAdMetrics builds the job.
func NewGoogleAdMetrics(db *ent.Client, keys *encryption.KeyProvider, logger *zap.Logger) *GoogleAdMetrics {
	return &GoogleAdMetrics{db: db, hc: &http.Client{Timeout: 30 * time.Second}, now: time.Now, logger: logger.Named("google-ad-metrics"),
		tokens: google.NewTokensFor(db, keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName),
		extra: func(ctx context.Context) (map[string]string, error) {
			_, extra, err := google.LoadClient(ctx, db, keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName)
			return extra, err
		}}
}

// Run blocks until ctx is cancelled.
func (j *GoogleAdMetrics) Run(ctx context.Context) {
	t := time.NewTicker(googleMetricsEvery)
	defer t.Stop()
	for {
		if sharedcache.ClaimPeriod(ctx, "marketflow:google-ad-metrics", googleMetricsEvery) {
			j.sync(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sync reads every created Google campaign that can still change.
func (j *GoogleAdMetrics) sync(ctx context.Context) {
	rows, err := j.db.Campaign.Query().
		Where(campaign.PlatformEQ("google"), campaign.ConnectedAccountIDNotNil(),
			campaign.Or(campaign.EndDateIsNil(), campaign.EndDateGTE(j.now().Add(-googleMetricsAfter))),
			func(s *entsql.Selector) {
				s.Where(entsql.P(func(b *entsql.Builder) {
					b.WriteString(s.C(campaign.FieldMetadata) + "->'google'->>'campaign_resource' <> ''")
				}))
			}).
		Order(ent.Asc(campaign.FieldCreatedAt)).Limit(googleMetricsBatch).All(ctx)
	if err != nil {
		j.logger.Warn("google campaigns not read", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}
	extra, err := j.extra(ctx)
	if err != nil {
		return // the platform Google Ads client is not configured
	}
	for _, c := range rows {
		if err := j.syncCampaign(ctx, c, extra); err != nil {
			j.logger.Warn("google campaign results not stored", zap.String("campaign", c.ID.String()), zap.Error(err))
		}
	}
}

func (j *GoogleAdMetrics) syncCampaign(ctx context.Context, c *ent.Campaign, extra map[string]string) error {
	acc, err := j.db.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(*c.ConnectedAccountID), connectedaccount.TenantIDEQ(c.TenantID),
			connectedaccount.AccountTypeEQ(googleads.AccountType), connectedaccount.StatusEQ("active")).Only(ctx)
	if ent.IsNotFound(err) {
		return nil // disconnected: what is stored stays
	}
	if err != nil {
		return err
	}
	tok, err := j.tokens.AccessToken(ctx, acc)
	if err != nil {
		return err
	}
	ads := google.AdsFromSettings(j.hc, extra, tok)
	login, _ := acc.Metadata["login_customer_id"].(string)
	ads.LoginCustomerID = google.CustomerDigits(login)
	if j.base != "" {
		ads.Base = j.base
	}
	gm, _ := c.Metadata["google"].(map[string]any)
	resource, _ := gm["campaign_resource"].(string)
	cur, _ := acc.Metadata["currency"].(string)
	if cur == "" {
		cur = c.Currency
	}
	return j.store(ctx, c, cur, ads, acc.ExternalID, resource)
}

// store reads the campaign's recent days and writes one row per day.
func (j *GoogleAdMetrics) store(ctx context.Context, c *ent.Campaign, cur string, ads google.Ads, customer, resource string) error {
	days, err := ads.CampaignDays(ctx, customer, resource, j.now().AddDate(0, 0, -googleMetricsBack), j.now())
	if err != nil {
		return err
	}
	for _, d := range days {
		leads := int64(d.Conversions)
		var ctr, cpl float64
		if d.Impressions > 0 {
			ctr = float64(d.Clicks) / float64(d.Impressions) * 100
		}
		if leads > 0 {
			cpl = d.Cost / float64(leads)
		}
		if err := j.db.AdMetrics.Create().SetTenantID(c.TenantID).SetCampaignID(c.ID).SetPlatform("google").
			SetExternalCampaignID(d.ID).SetDate(d.Date).SetImpressions(d.Impressions).SetClicks(d.Clicks).
			SetLeads(leads).SetSpend(d.Cost).SetCtr(ctr).SetCpl(cpl).SetCurrency(cur).SetFetchedAt(j.now().UTC()).
			OnConflictColumns(admetrics.FieldCampaignID, admetrics.FieldDate).UpdateNewValues().Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}
