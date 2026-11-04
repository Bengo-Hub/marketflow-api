package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"go.uber.org/zap"
)

// CompetitorScan runs each tenant's competitor ad scan once a week (Sprint 24). marketflow-ai does
// the searching inside SerpApi's free quota; the winners land in Tenant.metadata.competitor_scan
// for the settings page and the Ads Specialist. Results are public ad data, never tenant PII.
type CompetitorScan struct {
	db     *ent.Client
	http   *http.Client
	aiURL  string
	aiKey  string
	logger *zap.Logger
	claim  func(ctx context.Context, key string, ttl time.Duration) bool
	now    func() time.Time
}

func NewCompetitorScan(db *ent.Client, aiURL, aiKey string, logger *zap.Logger) *CompetitorScan {
	return &CompetitorScan{db: db, http: &http.Client{Timeout: 3 * time.Minute}, aiURL: strings.TrimSuffix(aiURL, "/"), aiKey: aiKey,
		logger: logger.Named("competitor-scan"), claim: sharedcache.ClaimOnce, now: time.Now}
}

// Run checks every 6 hours; each tenant is scanned once per ISO week (the claim key), so a
// restart or a second pod never scans twice.
func (c *CompetitorScan) Run(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *CompetitorScan) tick(ctx context.Context) {
	rows, err := c.db.Tenant.Query().
		Where(func(s *entsql.Selector) {
			s.Where(entsql.ExprP("jsonb_array_length(COALESCE(" + s.C(tenant.FieldMetadata) + "->'" + marketingos.CompetitorsKey + "', '[]'::jsonb)) > 0"))
		}).
		Limit(500).All(ctx)
	if err != nil {
		c.logger.Warn("load tenants with competitors", zap.Error(err))
		return
	}
	year, week := c.now().UTC().ISOWeek()
	for _, t := range rows {
		key := fmt.Sprintf("marketflow:competitor-scan:%s:%d-%02d", t.ID, year, week)
		if !c.claim(ctx, key, 8*24*time.Hour) {
			continue
		}
		if quotaUsed, err := c.scan(ctx, t); err != nil {
			c.logger.Warn("competitor scan", zap.String("tenant", t.ID.String()), zap.Error(err))
		} else if quotaUsed {
			return // the month's pool is used; nobody else gets searches this tick
		}
	}
}

type scanResult struct {
	Results []struct {
		Query string `json:"query"`
		Ads   []struct {
			Advertiser   string  `json:"advertiser"`
			Format       string  `json:"format"`
			DaysShown    int     `json:"total_days_shown"`
			StillRunning bool    `json:"still_running"`
			Score        float64 `json:"score"`
			DetailsLink  string  `json:"details_link"`
		} `json:"ads"`
		Error string `json:"error"`
	} `json:"results"`
	QuotaUsed bool `json:"quota_used"`
}

func (c *CompetitorScan) scan(ctx context.Context, t *ent.Tenant) (bool, error) {
	var queries []string
	if list, ok := t.Metadata[marketingos.CompetitorsKey].([]any); ok {
		for _, q := range list {
			if s, ok := q.(string); ok && s != "" {
				queries = append(queries, s)
			}
		}
	}
	body, _ := json.Marshal(map[string]any{"tenant_id": t.ID.String(), "queries": queries})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.aiURL+"/api/v1/internal/adintel/scan", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.aiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("adintel scan: status %d", resp.StatusCode)
	}
	var out scanResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	// Keep a small summary: the top 5 winners per competitor.
	summary := map[string]any{"scanned_at": c.now().UTC().Format(time.RFC3339), "quota_used": out.QuotaUsed}
	per := []map[string]any{}
	for _, r := range out.Results {
		ads := r.Ads
		if len(ads) > 5 {
			ads = ads[:5]
		}
		per = append(per, map[string]any{"query": r.Query, "winners": ads, "error": r.Error})
	}
	summary["competitors"] = per
	patch, _ := json.Marshal(map[string]any{"competitor_scan": summary})
	value := entsql.ExprFunc(func(bd *entsql.Builder) {
		bd.WriteString("(COALESCE(" + tenant.FieldMetadata + ", '{}'::jsonb) || ")
		bd.Arg(string(patch))
		bd.WriteString("::jsonb)")
	})
	_, err = c.db.Tenant.Update().Where(tenant.IDEQ(t.ID)).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(tenant.FieldMetadata, value) }).Save(ctx)
	return out.QuotaUsed, err
}
