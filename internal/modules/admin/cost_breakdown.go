package admin

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Breakdowns of the monthly cost report (owner, 2026-11-24): the same month's creative versions
// by feature (poster, motion poster, video ad), by provider route, by day, and the platform
// owner's own spend, which is real provider money with no credits charged. Every query is
// bounded by the month on creative_versions.created_at (indexed) and reuses versionCostCTE, so
// the figures add up to the report's totals.

// costBreakdownLine is one row of a breakdown.
type costBreakdownLine struct {
	Key string `json:"key"`
	costLine
}

// costDay is one day of the month's trend.
type costDay struct {
	Day string `json:"day"` // YYYY-MM-DD, UTC
	costLine
}

// platformOwnerLine is the platform owner's own spend (it is never charged credits).
type platformOwnerLine struct {
	Slug string `json:"slug"`
	costLine
}

// costByFeatureSQL is the month by version kind.
const costByFeatureSQL = versionCostCTE + `
SELECT kind, COUNT(*), COALESCE(SUM(credits), 0)::bigint, COALESCE(SUM(cost_usd), 0)::float8
FROM billed GROUP BY kind ORDER BY 4 DESC, 1`

// costDailySQL is the month by UTC day.
const costDailySQL = versionCostCTE + `
SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, COUNT(*), COALESCE(SUM(credits), 0)::bigint, COALESCE(SUM(cost_usd), 0)::float8
FROM billed GROUP BY 1 ORDER BY 1`

// costMonthlySQL is the six months up to the report's month, by UTC month.
const costMonthlySQL = versionCostCTE + `
SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM') AS month, COUNT(*), COALESCE(SUM(credits), 0)::bigint, COALESCE(SUM(cost_usd), 0)::float8
FROM billed GROUP BY 1 ORDER BY 1`

// costPlatformOwnerSQL is the platform owner's own versions of the month ($3 is its slug).
const costPlatformOwnerSQL = versionCostCTE + `
SELECT COUNT(*), COALESCE(SUM(b.credits), 0)::bigint, COALESCE(SUM(b.cost_usd), 0)::float8
FROM billed b JOIN tenants t ON t.id = b.tenant_id
WHERE t.slug = $3`

// costByRouteSQL is the provider cost by route (provider and model) the month's media ran on:
// a poster's or motion poster's own route, and each scene a video ad made (its scene cache),
// leaving out scenes reused from an earlier version, whose cost was counted there. Top 20.
const costByRouteSQL = `
WITH x AS (
	SELECT cv.metadata->>'route' AS route,
		CASE WHEN jsonb_typeof(cv.metadata->'cost_usd') = 'number' THEN (cv.metadata->>'cost_usd')::numeric END AS cost
	FROM creative_versions cv
	WHERE cv.created_at >= $1 AND cv.created_at < $2 AND cv.kind <> 'video_ad'
	UNION ALL
	SELECT sc->>'route',
		CASE WHEN jsonb_typeof(sc->'cost_usd') = 'number' THEN (sc->>'cost_usd')::numeric END
	FROM creative_versions cv,
		jsonb_array_elements(CASE WHEN jsonb_typeof(cv.metadata->'scene_cache') = 'array' THEN cv.metadata->'scene_cache' ELSE '[]'::jsonb END) sc
	WHERE cv.created_at >= $1 AND cv.created_at < $2 AND cv.kind = 'video_ad'
		AND COALESCE(sc->>'reused', 'false') <> 'true'
)
SELECT route, COUNT(*), COALESCE(SUM(cost), 0)::float8
FROM x WHERE COALESCE(route, '') <> '' AND COALESCE(cost, 0) > 0
GROUP BY route ORDER BY 3 DESC, 1 LIMIT 20`

// costBreakdowns is the raw breakdown sums.
type costBreakdowns struct {
	ByFeature []costBreakdownSum
	Daily     []costBreakdownSum
	ByRoute   []costBreakdownSum
	Monthly   []costBreakdownSum
	Owner     *monthSums
}

// costBreakdownSum is one grouped sum.
type costBreakdownSum struct {
	Key      string
	Versions int64
	Credits  int64
	CostUSD  float64
}

func scanGroups(ctx context.Context, db *sql.DB, query string, withCredits bool, args ...any) ([]costBreakdownSum, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []costBreakdownSum{}
	for rows.Next() {
		var s costBreakdownSum
		var err error
		if withCredits {
			err = rows.Scan(&s.Key, &s.Versions, &s.Credits, &s.CostUSD)
		} else {
			err = rows.Scan(&s.Key, &s.Versions, &s.CostUSD)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// queryCostBreakdowns runs the breakdown queries for [start, end). platformSlug "" skips the
// platform owner's line.
func queryCostBreakdowns(ctx context.Context, db *sql.DB, start, end time.Time, platformSlug string) (costBreakdowns, error) {
	var out costBreakdowns
	var err error
	if out.ByFeature, err = scanGroups(ctx, db, costByFeatureSQL, true, start, end); err != nil {
		return out, fmt.Errorf("cost by feature: %w", err)
	}
	if out.Daily, err = scanGroups(ctx, db, costDailySQL, true, start, end); err != nil {
		return out, fmt.Errorf("cost by day: %w", err)
	}
	if out.ByRoute, err = scanGroups(ctx, db, costByRouteSQL, false, start, end); err != nil {
		return out, fmt.Errorf("cost by route: %w", err)
	}
	if out.Monthly, err = scanGroups(ctx, db, costMonthlySQL, true, end.AddDate(0, -6, 0), end); err != nil {
		return out, fmt.Errorf("cost by month: %w", err)
	}
	if platformSlug != "" {
		var o monthSums
		if err := db.QueryRowContext(ctx, costPlatformOwnerSQL, start, end, platformSlug).Scan(&o.Versions, &o.Credits, &o.CostUSD); err != nil {
			return out, fmt.Errorf("platform owner spend: %w", err)
		}
		out.Owner = &o
	}
	return out, nil
}

// WithPlatformSlug names the platform owner's tenant, whose own spend the report shows apart.
func (h *CostReportHandler) WithPlatformSlug(slug string) *CostReportHandler {
	h.platformSlug = slug
	return h
}
