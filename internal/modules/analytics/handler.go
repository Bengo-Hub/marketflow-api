// Package analytics serves the tenant dashboard aggregate query.
package analytics

import (
	"context"
	"net/http"
	"sort"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"golang.org/x/sync/errgroup"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/chatsession"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves tenant-scoped aggregate stats for the dashboard.
type Handler struct {
	db     *ent.Client
	cache  *sharedcache.Aside
	logger *zap.Logger
}

func NewHandler(db *ent.Client, logger *zap.Logger) *Handler {
	return &Handler{db: db, logger: logger.Named("analytics")}
}

// WithCache caches the dashboard per tenant for a minute. Without it every request
// queries the database.
func (h *Handler) WithCache(c *sharedcache.Aside) *Handler {
	h.cache = c
	return h
}

func extractTenant(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(r.Header.Get("X-Tenant-ID"))
}

// PlatformUsage returns aggregate usage across all tenants (platform admin).
// @Summary Platform-wide usage rollup
// @Tags Admin - Analytics
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/usage [get]
func (h *Handler) PlatformUsage(w http.ResponseWriter, r *http.Request) {
	// The platform page polls every minute and lead counts grow with every tenant, so the
	// aggregate is computed at most once a minute, shared by every platform-owner session.
	data, err := sharedcache.GetOrSet(r.Context(), h.cache, sharedcache.Key("mf", "analytics", "platform-usage"), dashboardCacheTTL, h.platformUsage)
	if err != nil {
		h.logger.Error("platform usage query failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "could not load platform usage", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, data)
}

// platformUsage counts across every tenant in parallel and ranks this week's top ten by leads.
func (h *Handler) platformUsage(ctx context.Context) (map[string]any, error) {
	now := time.Now().UTC()
	weekAgo := now.Add(-7 * 24 * time.Hour)

	var tenantCount, activeTenants, leadsTotal, leadsThisWeek, chatThisWeek, meetingsUpcoming, enrollmentsActive, campaignsActive int
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	count := func(dst *int, q func(context.Context) (int, error)) {
		g.Go(func() (err error) { *dst, err = q(gctx); return err })
	}
	count(&tenantCount, h.db.Tenant.Query().Count)
	count(&activeTenants, h.db.Tenant.Query().Where(tenant.StatusEQ("active")).Count)
	count(&leadsTotal, h.db.Lead.Query().Count)
	count(&leadsThisWeek, h.db.Lead.Query().Where(lead.CreatedAtGTE(weekAgo)).Count)
	count(&chatThisWeek, h.db.ChatSession.Query().Where(chatsession.StartedAtGTE(weekAgo)).Count)
	count(&meetingsUpcoming, h.db.ScheduledMeeting.Query().
		Where(scheduledmeeting.StatusEQ("scheduled"), scheduledmeeting.ScheduledAtGTE(now)).Count)
	count(&enrollmentsActive, h.db.NurtureEnrollment.Query().Where(nurtureenrollment.StatusEQ("active")).Count)
	count(&campaignsActive, h.db.Campaign.Query().Where(campaign.StatusEQ("active")).Count)

	// Per-tenant top-10 leaderboard by lead count (last 7d).
	type row struct {
		TenantID   string `json:"tenant_id"`
		TenantName string `json:"tenant_name"`
		Slug       string `json:"slug"`
		Leads      int    `json:"leads_this_week"`
	}
	// One grouped query for the top ten, then one lookup for their names. This
	// used to load every tenant and count each one's leads separately.
	var top []struct {
		TenantID uuid.UUID `json:"tenant_id"`
		Count    int       `json:"count"`
	}
	g.Go(func() error {
		return h.db.Lead.Query().
			Where(lead.CreatedAtGTE(weekAgo)).
			GroupBy(lead.FieldTenantID).
			Aggregate(ent.Count()).
			Scan(gctx, &top)
	})
	// Creative Studio files not yet purged, by kind: drafts still expire, kept
	// files belong to approved versions.
	var drafts, kept []storageRow
	storage := func(dst *[]storageRow, state predicate.CreativeAsset) {
		g.Go(func() error {
			return h.db.CreativeAsset.Query().
				Where(creativeasset.PurgedAtIsNil(), state).
				GroupBy(creativeasset.FieldKind).
				Aggregate(ent.As(ent.Sum(creativeasset.FieldBytes), "bytes"), ent.As(ent.Count(), "files")).
				Scan(gctx, dst)
		})
	}
	storage(&drafts, creativeasset.ExpiresAtNotNil())
	storage(&kept, creativeasset.ExpiresAtIsNil())
	if err := g.Wait(); err != nil {
		return nil, err
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Count > top[j].Count })
	if len(top) > 10 {
		top = top[:10]
	}
	ids := make([]uuid.UUID, 0, len(top))
	for _, t := range top {
		ids = append(ids, t.TenantID)
	}
	names := map[uuid.UUID]*ent.Tenant{}
	if len(ids) > 0 {
		rows, err := h.db.Tenant.Query().Where(tenant.IDIn(ids...)).All(ctx)
		if err != nil {
			h.logger.Warn("leaderboard tenant names failed", zap.Error(err))
		}
		for _, t := range rows {
			names[t.ID] = t
		}
	}
	leaderboard := make([]row, 0, len(top))
	for _, t := range top {
		r := row{TenantID: t.TenantID.String(), Leads: t.Count}
		if tn := names[t.TenantID]; tn != nil {
			r.TenantName, r.Slug = tn.Name, tn.Slug
		}
		leaderboard = append(leaderboard, r)
	}

	return map[string]any{
		"tenants": map[string]any{
			"total":  tenantCount,
			"active": activeTenants,
		},
		"leads": map[string]any{
			"total":     leadsTotal,
			"this_week": leadsThisWeek,
		},
		"chat_sessions": map[string]any{
			"this_week": chatThisWeek,
		},
		"campaigns": map[string]any{
			"active": campaignsActive,
		},
		"meetings": map[string]any{
			"upcoming": meetingsUpcoming,
		},
		"nurture": map[string]any{
			"active_enrollments": enrollmentsActive,
		},
		"creative_storage": creativeStorage(drafts, kept),
		"leaderboard":      leaderboard,
		"generated_at":     now.Format(time.RFC3339),
	}, nil
}
