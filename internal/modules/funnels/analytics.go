package funnels

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// AnalyticsHandler handles funnel analytics HTTP requests.
type AnalyticsHandler struct {
	service *Service
	redis   *redis.Client
	db      *ent.Client
	logger  *zap.Logger
}

// NewAnalyticsHandler creates a new funnel analytics handler.
func NewAnalyticsHandler(service *Service, redisClient *redis.Client, db *ent.Client, logger *zap.Logger) *AnalyticsHandler {
	return &AnalyticsHandler{
		service: service,
		redis:   redisClient,
		db:      db,
		logger:  logger.Named("funnels.analytics"),
	}
}

// stepViewRedisKey returns the Redis key for a funnel step view counter.
func stepViewRedisKey(slug string, stepIndex int) string {
	return fmt.Sprintf("mf:funnel:%s:step:%d", slug, stepIndex)
}

type stepViewRequest struct {
	StepIndex int `json:"step_index"`
}

// RecordStepView handles POST /api/v1/public/funnels/{slug}/step-view.
// Public, no auth, rate-limited at the ingress layer. Increments a Redis
// counter for the given funnel slug + step index.
// @Summary Record a funnel step view
// @Description Increments the view counter for a specific funnel step (public, no auth)
// @Tags Public
// @Accept json
// @Produce json
// @Param slug path string true "Funnel slug"
// @Param request body stepViewRequest true "Step view data"
// @Success 204
// @Failure 400 {object} httpapi.ErrorResponse
// @Router /public/funnels/{slug}/step-view [post]
func (h *AnalyticsHandler) RecordStepView(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return
	}

	var req stepViewRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid JSON body", nil)
		return
	}
	if req.StepIndex < 0 || req.StepIndex > 50 {
		httpapi.Error(w, http.StatusBadRequest, "invalid_step", "step_index must be 0-50", nil)
		return
	}

	key := stepViewRedisKey(slug, req.StepIndex)
	if err := h.redis.Incr(r.Context(), key).Err(); err != nil {
		h.logger.Warn("redis incr failed for step view", zap.String("key", key), zap.Error(err))
		// Fail silently — analytics failures must not break the visitor UX.
	}

	w.WriteHeader(http.StatusNoContent)
}

// GetAnalytics handles GET /api/v1/funnels/{id}/analytics.
// Returns per-step view counts and lead submission count for the funnel,
// along with drop-off between consecutive steps.
// @Summary Funnel step drop-off analytics
// @Description Returns view counts per step and drop-off between steps
// @Tags Funnels
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Funnel UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /funnels/{id}/analytics [get]
func (h *AnalyticsHandler) GetAnalytics(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	funnelID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid funnel ID", nil)
		return
	}

	f, err := h.service.Get(r.Context(), tenantID, funnelID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "funnel not found", nil)
		return
	}

	// Determine the number of steps from the funnel's steps JSON.
	numSteps := len(f.Steps)
	if numSteps == 0 {
		numSteps = 1 // at least one "page"
	}

	ctx := r.Context()
	steps := make([]map[string]any, 0, numSteps)
	for i := 0; i < numSteps; i++ {
		views := getStepViews(ctx, h.redis, f.Slug, i)
		step := map[string]any{
			"step_index": i,
			"views":      views,
		}
		if i > 0 {
			prevViews := getStepViews(ctx, h.redis, f.Slug, i-1)
			dropoff := int64(0)
			if prevViews > 0 {
				dropoff = prevViews - views
			}
			step["dropoff"] = dropoff
			dropoffRate := 0.0
			if prevViews > 0 {
				dropoffRate = float64(dropoff) / float64(prevViews) * 100
			}
			step["dropoff_rate"] = dropoffRate
		}
		steps = append(steps, step)
	}

	// Lead submission count for this funnel.
	leadCount, _ := h.db.Lead.Query().
		Where(
			lead.TenantIDEQ(tenantID),
			lead.FunnelIDEQ(funnelID),
		).Count(ctx)

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"funnel_id":   funnelID,
		"slug":        f.Slug,
		"total_steps": numSteps,
		"steps":       steps,
		"lead_submissions": leadCount,
	})
}

// getStepViews reads the step view counter from Redis.
func getStepViews(ctx context.Context, rdb *redis.Client, slug string, stepIndex int) int64 {
	key := stepViewRedisKey(slug, stepIndex)
	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(val, 10, 64)
	return n
}
