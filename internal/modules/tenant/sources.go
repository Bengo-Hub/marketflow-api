package tenant

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
)

// SettingSource is one setting another service owns: where it comes from, when it was last read
// and how to read it again (plan item 75).
type SettingSource struct {
	Key string
	// Name and From are what the settings page shows.
	Name, From string
	// Updates says how changes arrive when nobody syncs by hand.
	Updates string
	// State reads the current value; fresh goes to the owning service past any cache.
	State func(ctx context.Context, t *ent.Tenant, fresh bool) (SourceState, error)
}

// SourceState is a source's current value in words and when it was read.
type SourceState struct {
	Detail   string
	LastSync time.Time
}

type sourceView struct {
	Key      string     `json:"key"`
	Name     string     `json:"name"`
	From     string     `json:"from"`
	Updates  string     `json:"updates"`
	Detail   string     `json:"detail,omitempty"`
	LastSync *time.Time `json:"last_sync,omitempty"`
	OK       bool       `json:"ok"`
}

// sourceTimeout bounds one read of another service.
const sourceTimeout = 6 * time.Second

func (h *Handler) readSource(ctx context.Context, s SettingSource, t *ent.Tenant, fresh bool) (sourceView, error) {
	v := sourceView{Key: s.Key, Name: s.Name, From: s.From, Updates: s.Updates}
	ctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()
	st, err := s.State(ctx, t, fresh)
	if err != nil {
		h.logger.Warn("setting source unavailable", zap.String("source", s.Key), zap.Error(err))
		return v, err
	}
	v.OK, v.Detail = true, st.Detail
	if !st.LastSync.IsZero() {
		at := st.LastSync.UTC()
		v.LastSync = &at
	}
	return v, nil
}

// ListSources handles GET /api/v1/tenant/profile/sources: the settings other services own, each
// with its source and last sync. A source that cannot answer is listed with ok false.
// @Summary Settings that come from other services
// @Tags Tenant
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/profile/sources [get]
func (h *Handler) ListSources(w http.ResponseWriter, r *http.Request) {
	t, ok := h.sourceTenant(w, r)
	if !ok {
		return
	}
	out := make([]sourceView, len(h.Sources))
	var wg sync.WaitGroup
	for i, s := range h.Sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], _ = h.readSource(r.Context(), s, t, false)
		}()
	}
	wg.Wait()
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out})
}

// SyncSource handles POST /api/v1/tenant/profile/sources/{key}/sync: reads one source again now.
// An admin's, like every write under /tenant/profile (the role gate).
// @Summary Sync one setting from its source
// @Tags Tenant
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param key path string true "Source key"
// @Success 200 {object} map[string]interface{}
// @Failure 502 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/profile/sources/{key}/sync [post]
func (h *Handler) SyncSource(w http.ResponseWriter, r *http.Request) {
	t, ok := h.sourceTenant(w, r)
	if !ok {
		return
	}
	key := chi.URLParam(r, "key")
	for _, s := range h.Sources {
		if s.Key != key {
			continue
		}
		v, err := h.readSource(r.Context(), s, t, true)
		if err != nil {
			httpapi.Error(w, http.StatusBadGateway, "source_unavailable", s.From+" did not answer. The last values stay in use; try again shortly.", nil)
			return
		}
		audit.Tenant(r.Context(), h.db, h.logger, t.ID, "settings.synced", "setting_source", nil, map[string]any{"source": key})
		httpapi.JSON(w, http.StatusOK, v)
		return
	}
	httpapi.Error(w, http.StatusNotFound, "not_found", "unknown source", nil)
}

func (h *Handler) sourceTenant(w http.ResponseWriter, r *http.Request) (*ent.Tenant, bool) {
	id, ok := tenantHeader(w, r)
	if !ok {
		return nil, false
	}
	t, err := h.db.Tenant.Get(r.Context(), id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "tenant not found", nil)
		return nil, false
	}
	return t, true
}
