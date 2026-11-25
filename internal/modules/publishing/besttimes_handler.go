package publishing

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/publishing/postingplan"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
)

// bestTimesPlatform is one platform's settings, its next best time and its posts so far.
type bestTimesPlatform struct {
	postingplan.Platform
	Next   time.Time          `json:"next_best_time"`
	Booked postingplan.Counts `json:"booked"`
	// Due is true while the cadence wants another post this week (or month).
	Due bool `json:"due"`
}

// BestTimes handles GET /api/v1/publishing/best-times: the tenant's posting windows and
// cadence per platform (with where each comes from), the next best time and this week's and
// month's posts, and the researched defaults for "reset".
// @Summary Posting times and cadence
// @Tags Publishing
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /publishing/best-times [get]
func (h *Handler) BestTimes(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	s := postingplan.Load(ctx, h.Store.DB, tid)
	now := time.Now()
	booked, err := postingplan.Booked(ctx, h.Store.DB, tid, postingplan.Platforms, postingplan.BookedSince(now, s.Loc))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := map[string]bestTimesPlatform{}
	for _, p := range postingplan.Platforms {
		pl := s.Platforms[p]
		out[p] = bestTimesPlatform{
			Platform: pl,
			Next:     postingplan.Slot(pl, now, s.Loc, booked[p]),
			Booked:   postingplan.Count(booked[p], now, s.Loc),
			Due:      postingplan.Due(pl.Cadence, booked[p], now, s.Loc),
		}
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"timezone":  s.Timezone,
		"platforms": out,
		// What a business that set nothing gets, so the tick shows what really happens.
		"best_time_default": bestTimeOn(ctx, h.Store.DB, nil),
		"defaults":          map[string]any{"windows": postingplan.DefaultWindows(), "cadence": postingplan.DefaultCadence()},
	})
}

// bestTimesInput is the body of PUT /publishing/best-times. A platform left out keeps its
// current choice; a platform set to null (or an empty list) goes back to the platform default.
type bestTimesInput struct {
	Windows map[string]json.RawMessage `json:"windows"`
	Cadence map[string]json.RawMessage `json:"cadence"`
}

// SaveBestTimes handles PUT /api/v1/publishing/best-times: the tenant's own windows and cadence,
// kept in Tenant.metadata (posting_times, posting_cadence). Owners and admins only.
// @Summary Save posting times and cadence
// @Tags Publishing
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /publishing/best-times [put]
func (h *Handler) SaveBestTimes(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok || !h.allowed(w, r, tid) {
		return
	}
	var in bestTimesInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	ctx := r.Context()
	t, err := h.Store.DB.Tenant.Get(ctx, tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	windows, err := mergePerPlatform(t.Metadata[postingplan.TenantTimesKey], in.Windows)
	if err == nil {
		_, err = postingplan.ParseWindows(windows)
	}
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_posting_times", err.Error(), nil)
		return
	}
	cadence, err := mergePerPlatform(t.Metadata[postingplan.TenantCadenceKey], in.Cadence)
	if err == nil {
		_, err = postingplan.ParseCadence(cadence)
	}
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_posting_cadence", err.Error(), nil)
		return
	}
	patch := map[string]any{postingplan.TenantTimesKey: nilIfEmpty(windows), postingplan.TenantCadenceKey: nilIfEmpty(cadence)}
	if err := metastore.SetKeys(ctx, h.Store.DB, tid, patch); err != nil {
		h.fail(w, err)
		return
	}
	h.BestTimes(w, r)
}

// mergePerPlatform applies a per-platform update to the stored map: a value replaces the
// platform's entry, null or an empty list removes it.
func mergePerPlatform(stored any, update map[string]json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	if m, ok := stored.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	for p, raw := range update {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if list, isList := v.([]any); v == nil || (isList && len(list) == 0) {
			delete(out, p)
			continue
		}
		out[p] = v
	}
	return out, nil
}

func nilIfEmpty(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}
