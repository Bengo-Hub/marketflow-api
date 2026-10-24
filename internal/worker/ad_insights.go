package worker

import (
	"context"
	"database/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"go.uber.org/zap"
)

// AdInsights is a recurring cron that pulls per-campaign metrics from every
// supported ad platform and upserts one AdMetrics row per (campaign, date).
// Runs every 15 minutes; each execution is idempotent thanks to the unique
// (campaign_id, date) index. Only Meta is wired today — Google Ads /
// TikTok / LinkedIn / X polls can be added as platforms gain tenant adoption.
type AdInsights struct {
	db       *ent.Client
	interval time.Duration
	logger   *zap.Logger
	meta     *meta.Client
	resolver func(ctx context.Context, tenantID string) (accessToken string, ok bool)
}

// NewAdInsights constructs the insights cron.
// `resolver` returns the tenant's Meta page access token (already decrypted)
// so we don't couple the worker to ProviderSetting decoding logic.
func NewAdInsights(db *ent.Client, interval time.Duration, resolver func(ctx context.Context, tenantID string) (string, bool), logger *zap.Logger) *AdInsights {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	return &AdInsights{
		db:       db,
		interval: interval,
		meta:     meta.NewClient(""),
		resolver: resolver,
		logger:   logger.Named("ad-insights"),
	}
}

// WithMetaSecret makes the insights calls carry appsecret_proof (the Meta app
// requires it when "Require app secret" is on).
func (a *AdInsights) WithMetaSecret(secret meta.SecretSource) *AdInsights {
	a.meta.WithSecret(secret)
	return a
}

// Run blocks until ctx is cancelled, firing tick() every `interval`.
func (a *AdInsights) Run(ctx context.Context) {
	if a.db == nil || a.resolver == nil {
		a.logger.Warn("ad-insights cron disabled — DB or resolver missing")
		return
	}
	tk := time.NewTicker(a.interval)
	defer tk.Stop()
	a.logger.Info("ad-insights cron started", zap.Duration("interval", a.interval))
	// Run once immediately so K8s observers see activity quickly.
	a.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			a.tick(ctx)
		}
	}
}

func (a *AdInsights) tick(ctx context.Context) {
	// Runs on every replica's ticker; only the first replica in each period does the work.
	if !sharedcache.ClaimPeriod(ctx, "marketflow:ad-insights", a.interval) {
		return
	}
	// Only look at campaigns the tenant has actually wired up to Meta —
	// identified by a non-empty meta_campaign_id.
	rows, err := a.db.Campaign.Query().
		Where(
			campaign.PlatformEQ("meta"),
			campaign.StatusEQ("active"),
			campaign.MetaCampaignIDNEQ(""),
		).
		All(ctx)
	if err != nil {
		a.logger.Warn("query campaigns failed", zap.Error(err))
		return
	}
	for _, c := range rows {
		a.syncCampaign(ctx, c)
	}
}

func (a *AdInsights) syncCampaign(ctx context.Context, c *ent.Campaign) {
	token, ok := a.resolver(ctx, c.TenantID.String())
	if !ok || token == "" {
		return
	}
	if c.MetaCampaignID == nil || *c.MetaCampaignID == "" {
		return
	}
	metaCampaignID := *c.MetaCampaignID
	ins, err := a.meta.GetCampaignInsights(ctx, token, metaCampaignID)
	if err != nil {
		a.logger.Debug("insights fetch skipped",
			zap.String("campaign", c.ID.String()), zap.Error(err))
		return
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	impressions := parseInt64(ins.Impressions)
	clicks := parseInt64(ins.Clicks)
	spend := parseFloat(ins.Spend)
	leads := parseInt64(ins.Leads)
	ctr := parseFloat(ins.CTR)
	cpl := parseFloat(ins.CPL)
	currency := ins.Currency
	if currency == "" {
		currency = "USD"
	}

	// Upsert one row per (campaign, day). Ent's sql/upsert feature makes this
	// a single round-trip; we rely on the unique index on (campaign_id, date).
	err = a.db.AdMetrics.Create().
		SetTenantID(c.TenantID).
		SetCampaignID(c.ID).
		SetPlatform("meta").
		SetExternalCampaignID(metaCampaignID).
		SetDate(today).
		SetImpressions(impressions).
		SetClicks(clicks).
		SetSpend(spend).
		SetLeads(leads).
		SetCtr(ctr).
		SetCpl(cpl).
		SetCurrency(currency).
		OnConflictColumns(admetrics.FieldCampaignID, admetrics.FieldDate).
		UpdateNewValues().
		Exec(ctx)
	if err != nil {
		a.logger.Warn("ad_metrics upsert failed",
			zap.String("campaign", c.ID.String()), zap.Error(err))
		return
	}
	a.logger.Debug("ad_metrics upserted",
		zap.String("campaign", c.ID.String()),
		zap.Int64("impressions", impressions),
		zap.Int64("clicks", clicks),
		zap.Float64("spend", spend),
	)
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	var whole, frac int64
	var scale float64 = 1
	inFrac := false
	for _, r := range s {
		if r == '.' {
			inFrac = true
			continue
		}
		if r < '0' || r > '9' {
			break
		}
		if inFrac {
			frac = frac*10 + int64(r-'0')
			scale *= 10
		} else {
			whole = whole*10 + int64(r-'0')
		}
	}
	return float64(whole) + float64(frac)/scale
}

// Ensure sql import is referenced so we keep the dependency consistent with
// other workers that use *sql.DB for low-level queries.
var _ = (*sql.DB)(nil)
