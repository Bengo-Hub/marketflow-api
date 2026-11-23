package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// The platform cost and margin report (Sprint 15): for one calendar month, what each tenant's
// generated media cost the platform in provider fees against the AI credits it was charged for
// them. It reads only data already recorded: the creative versions (their credits and the
// provider cost marketflow-ai reported), the output registry and treasury's exchange rate.

// Defaults that mirror marketflow-ai's registry.Defaults(), used when the platform has stored no
// ai_registry/outputs override (or the override leaves a field out). Keep them in step with
// marketflow-ai internal/registry/registry.go.
const (
	defaultFXRate      = 129.0 // KES per USD, the fallback when treasury has no rate
	defaultMarginFloor = 0.30  // the lowest acceptable margin; lower is flagged
)

// defaultBundles are marketflow-ai's default credit bundles (subscriptions-api prices of
// 2026-11-11). The cheapest per credit is bundle_1000 at KES 10, the same as 100 credits for
// KES 1,000.
var defaultBundles = []creditBundle{
	{Code: "bundle_60", Units: 60, PriceKES: 750},
	{Code: "bundle_150", Units: 150, PriceKES: 1750},
	{Code: "bundle_400", Units: 400, PriceKES: 4400},
	{Code: "bundle_1000", Units: 1000, PriceKES: 10000},
}

// costReportGaps are the costs and revenues this report cannot see yet. The UI shows them so a
// margin is never read as the whole picture.
var costReportGaps = []string{
	"LLM chat and agent cost is not recorded per tenant",
	"chat-turn credits live in subscriptions-api",
}

const (
	defaultCostPageSize = 50
	maxCostPageSize     = 200
)

// creditBundle is one AI credit pack from the registry's bundle list.
type creditBundle struct {
	Code     string  `json:"code"`
	Units    int     `json:"units"`
	PriceKES float64 `json:"price_kes"`
}

// registryPricing is the part of the output registry the report needs.
type registryPricing struct {
	FXRate      float64
	MarginFloor float64
	Bundles     []creditBundle
}

// decodeRegistryPricing reads fx_rate, margin_floor and the bundle list from the stored
// ai_registry/outputs value ("registry" key) the way marketflow-ai merges it: a settings field
// that is set replaces the default, a non-empty bundle list replaces the default list. Only
// these fields are decoded; the rest of the registry belongs to marketflow-ai. An unreadable or
// implausible value keeps the default, as marketflow-ai keeps its last good registry.
func decodeRegistryPricing(raw string) (registryPricing, error) {
	out := registryPricing{FXRate: defaultFXRate, MarginFloor: defaultMarginFloor, Bundles: defaultBundles}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	var over struct {
		Settings struct {
			FXRate      *float64 `json:"fx_rate"`
			MarginFloor *float64 `json:"margin_floor"`
		} `json:"settings"`
		Bundles []creditBundle `json:"bundles"`
	}
	if err := json.Unmarshal([]byte(raw), &over); err != nil {
		return out, fmt.Errorf("registry: %w", err)
	}
	if v := over.Settings.FXRate; v != nil && *v > 0 {
		out.FXRate = *v
	}
	if v := over.Settings.MarginFloor; v != nil && *v >= 0 && *v < 1 {
		out.MarginFloor = *v
	}
	if len(over.Bundles) > 0 {
		out.Bundles = over.Bundles
	}
	return out, nil
}

// creditValueKES is what one credit brings in: the lowest price per credit of any bundle on
// sale (the biggest bundle), so a margin is never overstated by pricing every credit at the
// dearest pack. Zero when no bundle has a usable price.
func creditValueKES(bundles []creditBundle) float64 {
	best := 0.0
	for _, b := range bundles {
		if b.Units <= 0 || b.PriceKES <= 0 {
			continue
		}
		if per := b.PriceKES / float64(b.Units); best == 0 || per < best {
			best = per
		}
	}
	return best
}

// monthRange parses a YYYY-MM month (empty is the current UTC month) into its label and the
// half-open UTC range [start, next month start).
func monthRange(s string, now time.Time) (string, time.Time, time.Time, error) {
	var start time.Time
	if s = strings.TrimSpace(s); s == "" {
		n := now.UTC()
		start = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		t, err := time.Parse("2006-01", s)
		if err != nil || t.Year() < 2000 {
			return "", time.Time{}, time.Time{}, errors.New("month must be YYYY-MM")
		}
		start = t.UTC()
	}
	return start.Format("2006-01"), start, start.AddDate(0, 1, 0), nil
}

// parsePaging reads page (from 1) and page_size (1 to 200, default 50).
func parsePaging(page, size string) (int, int, error) {
	p, ps := 1, defaultCostPageSize
	if page != "" {
		n, err := strconv.Atoi(page)
		if err != nil || n < 1 {
			return 0, 0, errors.New("page must be 1 or more")
		}
		p = n
	}
	if size != "" {
		n, err := strconv.Atoi(size)
		if err != nil || n < 1 || n > maxCostPageSize {
			return 0, 0, fmt.Errorf("page_size must be between 1 and %d", maxCostPageSize)
		}
		ps = n
	}
	return p, ps, nil
}

// costLine is one row of the report: a tenant or the month's total.
type costLine struct {
	Versions       int64    `json:"versions"`
	CreditsCharged int64    `json:"credits_charged"`
	CreditsKES     float64  `json:"credits_kes"`
	MediaCostUSD   float64  `json:"media_cost_usd"`
	MediaCostKES   float64  `json:"media_cost_kes"`
	Margin         *float64 `json:"margin"`
	BelowFloor     bool     `json:"below_floor"`
}

// tenantCostLine is a tenant's row.
type tenantCostLine struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	costLine
}

// CostReport is the GET /admin/platform/cost-report body.
type CostReport struct {
	Month          string           `json:"month"`
	FXKESPerUSD    float64          `json:"fx_kes_per_usd"`
	FXSource       string           `json:"fx_source"` // treasury | registry
	CreditValueKES float64          `json:"credit_value_kes"`
	MarginFloor    float64          `json:"margin_floor"`
	Totals         costLine         `json:"totals"`
	Tenants        []tenantCostLine `json:"tenants"`
	Pagination     costPagination   `json:"pagination"`
	Gaps           []string         `json:"gaps"`
	// Breakdowns of the same month (cost_breakdown.go).
	ByFeature     []costBreakdownLine `json:"by_feature"`
	ByRoute       []costBreakdownLine `json:"by_route"`
	Daily         []costDay           `json:"daily"`
	Monthly       []costDay           `json:"monthly"` // the six months up to this one; day holds YYYY-MM
	PlatformOwner *platformOwnerLine  `json:"platform_owner,omitempty"`
}

type costPagination struct {
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// price turns the summed credits and provider cost into KES and the margin:
// (credits_kes - media_cost_kes) / credits_kes, null when nothing was charged. A margin under
// the floor is flagged, and so is any cost with no revenue against it. The margin is rounded to
// four places before the comparison, so a margin exactly at the floor is not flagged.
func price(versions, credits int64, costUSD, fx, creditKES, floor float64) costLine {
	l := costLine{
		Versions:       versions,
		CreditsCharged: credits,
		CreditsKES:     round(float64(credits)*creditKES, 2),
		MediaCostUSD:   round(costUSD, 4),
		MediaCostKES:   round(costUSD*fx, 2),
	}
	if l.CreditsKES <= 0 {
		l.BelowFloor = l.MediaCostKES > 0
		return l
	}
	m := round((l.CreditsKES-l.MediaCostKES)/l.CreditsKES, 4)
	l.Margin = &m
	l.BelowFloor = m < floor
	return l
}

func round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}

// versionCostCTE is each creative version of the month with its credits and recorded provider
// cost. It is shared by the page and the totals queries; $1 and $2 are the month's bounds.
//
// The range is on created_at as stored (no function on the column), so the
// creativeversion_created_at index serves it. A version's cost is the sum of what was recorded
// for it, each cast only when the JSON value is a number:
//   - cost_usd: the poster or the video render (jobs.go and render.go settle it on success);
//   - script_cost_usd: a video ad's script (video_ad.go);
//   - storyboard.cost_usd: a video ad's storyboard stills (storyboard.go). A revision
//     (ReviseVideoAd) copies its source's approved storyboard without paying for it again, so
//     that cost counts only when the storyboard was made after this version was created
//     (made_at is RFC 3339 UTC, compared as text with the version's creation second).
//
// Credits: creative_versions.credits only ever grows by a stage's credits_charged when that
// stage settles. A failed or lost job is refunded by marketflow-ai and never added, so the
// column is the credits charged and kept, whatever the version's status.
const versionCostCTE = `
WITH v AS (
	SELECT cv.tenant_id,
		cv.kind,
		cv.created_at,
		cv.credits,
		COALESCE(CASE WHEN jsonb_typeof(cv.metadata->'cost_usd') = 'number'
			THEN (cv.metadata->>'cost_usd')::numeric END, 0)
		+ COALESCE(CASE WHEN jsonb_typeof(cv.metadata->'script_cost_usd') = 'number'
			THEN (cv.metadata->>'script_cost_usd')::numeric END, 0)
		+ COALESCE(CASE WHEN jsonb_typeof(cv.metadata->'storyboard'->'cost_usd') = 'number'
			AND (cv.metadata->'storyboard'->>'made_at') >= to_char(cv.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
			THEN (cv.metadata->'storyboard'->>'cost_usd')::numeric END, 0) AS cost_usd
	FROM creative_versions cv
	WHERE cv.created_at >= $1 AND cv.created_at < $2
), billed AS (
	-- Only versions that were charged or cost something: a refunded failure with neither is
	-- not a version the platform paid for.
	SELECT * FROM v WHERE v.credits > 0 OR v.cost_usd > 0
)`

// costTotalsSQL is the month's totals and the number of tenants on the report.
const costTotalsSQL = versionCostCTE + `
SELECT COUNT(DISTINCT tenant_id), COUNT(*), COALESCE(SUM(credits), 0)::bigint, COALESCE(SUM(cost_usd), 0)::float8
FROM billed`

// costPageSQL is one page of tenants, the most costly first (tenant_id breaks ties so pages are
// stable). LIMIT/OFFSET is enough here: one row per tenant with activity that month.
const costPageSQL = versionCostCTE + `
, per_tenant AS (
	SELECT tenant_id, COUNT(*) AS versions, COALESCE(SUM(credits), 0)::bigint AS credits, COALESCE(SUM(cost_usd), 0) AS cost_usd
	FROM billed
	GROUP BY tenant_id
)
SELECT p.tenant_id, COALESCE(t.name, ''), COALESCE(t.slug, ''), p.versions, p.credits, p.cost_usd::float8
FROM per_tenant p
LEFT JOIN tenants t ON t.id = p.tenant_id
ORDER BY p.cost_usd DESC, p.tenant_id
LIMIT $3 OFFSET $4`

// tenantSums is one tenant's raw sums from the database.
type tenantSums struct {
	TenantID uuid.UUID
	Name     string
	Slug     string
	Versions int64
	Credits  int64
	CostUSD  float64
}

// monthSums is the month's raw totals.
type monthSums struct {
	Tenants  int64
	Versions int64
	Credits  int64
	CostUSD  float64
}

// queryCostSums runs the totals and the page queries for [start, end).
func queryCostSums(ctx context.Context, db *sql.DB, start, end time.Time, page, size int) (monthSums, []tenantSums, error) {
	var tot monthSums
	if err := db.QueryRowContext(ctx, costTotalsSQL, start, end).
		Scan(&tot.Tenants, &tot.Versions, &tot.Credits, &tot.CostUSD); err != nil {
		return tot, nil, fmt.Errorf("cost totals: %w", err)
	}
	rows, err := db.QueryContext(ctx, costPageSQL, start, end, size, (page-1)*size)
	if err != nil {
		return tot, nil, fmt.Errorf("cost page: %w", err)
	}
	defer rows.Close()
	out := []tenantSums{}
	for rows.Next() {
		var s tenantSums
		if err := rows.Scan(&s.TenantID, &s.Name, &s.Slug, &s.Versions, &s.Credits, &s.CostUSD); err != nil {
			return tot, nil, fmt.Errorf("cost page: %w", err)
		}
		out = append(out, s)
	}
	return tot, out, rows.Err()
}

// CostReportHandler serves the platform cost and margin report. Platform owners only (the
// /admin route group's gate).
type CostReportHandler struct {
	client *ent.Client
	db     *sql.DB
	// kesPer is treasury's KES per unit of a currency (fxrate.Rates.KESPer); nil or an error
	// falls back to the registry's fx_rate.
	kesPer func(ctx context.Context, currency string) (float64, error)
	logger *zap.Logger
	nowFn  func() time.Time
	// platformSlug is the platform owner's tenant, shown apart (WithPlatformSlug).
	platformSlug string
}

// NewCostReportHandler builds the report handler.
func NewCostReportHandler(client *ent.Client, db *sql.DB, kesPer func(ctx context.Context, currency string) (float64, error), logger *zap.Logger) *CostReportHandler {
	return &CostReportHandler{client: client, db: db, kesPer: kesPer, logger: logger.Named("admin.cost_report"), nowFn: time.Now}
}

// pricing reads the stored output registry (platform ai_registry/outputs, key "registry"). A
// missing row, an encrypted one (the registry is never a secret) or an unreadable value keeps
// marketflow-ai's defaults.
func (h *CostReportHandler) pricing(ctx context.Context) registryPricing {
	def, _ := decodeRegistryPricing("")
	row, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(platformTenantID),
			providersetting.ProviderTypeEQ("ai_registry"),
			providersetting.ProviderNameEQ("outputs"),
			providersetting.EnvironmentEQ("production"),
			providersetting.KeyEQ("registry"),
			providersetting.IsActiveEQ(true),
		).
		Order(ent.Desc(providersetting.FieldUpdatedAt)).
		First(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			h.logger.Warn("output registry not read; using defaults", zap.Error(err))
		}
		return def
	}
	if row.IsEncrypted {
		return def
	}
	p, err := decodeRegistryPricing(row.Value)
	if err != nil {
		h.logger.Warn("stored output registry is invalid; using defaults", zap.Error(err))
	}
	return p
}

// CostReport returns the month's provider cost against the credits charged, per tenant and in
// total, with the margin against the platform's margin floor.
//
// @Summary Platform cost and margin report (monthly)
// @Tags Admin - Platform
// @Produce json
// @Param month query string false "Month as YYYY-MM (default: the current UTC month)"
// @Param page query int false "Page, from 1"
// @Param page_size query int false "Tenants per page, 1 to 200 (default 50)"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/platform/cost-report [get]
func (h *CostReportHandler) CostReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	label, start, end, err := monthRange(q.Get("month"), h.nowFn())
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_month", err.Error(), nil)
		return
	}
	page, size, err := parsePaging(q.Get("page"), q.Get("page_size"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_paging", err.Error(), nil)
		return
	}
	ctx := r.Context()
	reg := h.pricing(ctx)
	fx, fxSource := reg.FXRate, "registry"
	if h.kesPer != nil {
		if rate, err := h.kesPer(ctx, "USD"); err == nil && rate > 0 {
			fx, fxSource = rate, "treasury"
		} else if err != nil {
			h.logger.Warn("treasury rate unavailable; using the registry rate", zap.Error(err))
		}
	}
	creditKES := creditValueKES(reg.Bundles)

	tot, rows, err := queryCostSums(ctx, h.db, start, end, page, size)
	if err != nil {
		h.logger.Error("cost report query failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "could not build the cost report", nil)
		return
	}
	report := CostReport{
		Month: label, FXKESPerUSD: fx, FXSource: fxSource, CreditValueKES: round(creditKES, 4), MarginFloor: reg.MarginFloor,
		Totals:     price(tot.Versions, tot.Credits, tot.CostUSD, fx, creditKES, reg.MarginFloor),
		Tenants:    make([]tenantCostLine, 0, len(rows)),
		Pagination: costPagination{Page: page, PageSize: size, Total: tot.Tenants},
		Gaps:       costReportGaps,
	}
	for _, s := range rows {
		report.Tenants = append(report.Tenants, tenantCostLine{
			TenantID: s.TenantID.String(), Name: s.Name, Slug: s.Slug,
			costLine: price(s.Versions, s.Credits, s.CostUSD, fx, creditKES, reg.MarginFloor),
		})
	}
	bd, err := queryCostBreakdowns(ctx, h.db, start, end, h.platformSlug)
	if err != nil {
		h.logger.Error("cost breakdowns failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "could not build the cost report", nil)
		return
	}
	line := func(s costBreakdownSum) costLine {
		return price(s.Versions, s.Credits, s.CostUSD, fx, creditKES, reg.MarginFloor)
	}
	report.ByFeature, report.ByRoute, report.Daily = []costBreakdownLine{}, []costBreakdownLine{}, []costDay{}
	for _, s := range bd.ByFeature {
		report.ByFeature = append(report.ByFeature, costBreakdownLine{Key: s.Key, costLine: line(s)})
	}
	for _, s := range bd.ByRoute {
		report.ByRoute = append(report.ByRoute, costBreakdownLine{Key: s.Key, costLine: line(s)})
	}
	for _, s := range bd.Daily {
		report.Daily = append(report.Daily, costDay{Day: s.Key, costLine: line(s)})
	}
	report.Monthly = []costDay{}
	for _, s := range bd.Monthly {
		report.Monthly = append(report.Monthly, costDay{Day: s.Key, costLine: line(s)})
	}
	if o := bd.Owner; o != nil {
		report.PlatformOwner = &platformOwnerLine{Slug: h.platformSlug, costLine: price(o.Versions, o.Credits, o.CostUSD, fx, creditKES, reg.MarginFloor)}
	}
	httpapi.JSON(w, http.StatusOK, report)
}
