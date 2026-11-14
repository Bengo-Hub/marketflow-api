package creative

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"go.uber.org/zap"
)

// The Studio's template picker (owner, 2026-11-14): the ad templates live in marketflow-ai's
// library (ad_templates.yaml); this route passes the list through, cached, so a template added
// there shows in the picker without a change here or in the portal.

// adTemplatesTTL is how long the list is kept; it changes only with a marketflow-ai deploy.
const adTemplatesTTL = 15 * time.Minute

type adTemplatesCache struct {
	mu   sync.Mutex
	body json.RawMessage
	at   time.Time
}

var templatesCache adTemplatesCache

// errNoAI is a template list that cannot be read from marketflow-ai.
var errNoAI = errors.New("marketflow-ai did not send the ad templates")

// AdTemplates handles GET /api/v1/creative/ad-templates.
// @Summary The ad templates a video ad script can follow
// @Description Every ad template (key, name, when to use it, the video kinds it suits, whether it can use two voices, what it needs) and each video kind's default, from marketflow-ai's template library.
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]any
// @Failure 503 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/ad-templates [get]
func (h *Handler) AdTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantOf(w, r); !ok {
		return
	}
	body, err := h.adTemplates(r.Context())
	if err != nil {
		h.logger.Warn("ad templates unavailable", zap.Error(err))
		httpapi.Error(w, http.StatusServiceUnavailable, "templates_unavailable", "The ad templates could not be loaded right now", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(body)
}

func (h *Handler) adTemplates(ctx context.Context) (json.RawMessage, error) {
	c := &templatesCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body != nil && time.Since(c.at) < adTemplatesTTL {
		return c.body, nil
	}
	if h.jobs == nil {
		return nil, errNoAI
	}
	data, status, err := h.jobs.aiCall(ctx, http.MethodGet, "/api/v1/internal/creative/ad-templates", nil, 10*time.Second)
	if err == nil && status != http.StatusOK {
		err = aiStatusError(status, data)
	}
	if err != nil {
		if c.body != nil {
			return c.body, nil // an old list beats none
		}
		return nil, err
	}
	if !json.Valid(data) {
		return nil, errNoAI
	}
	c.body, c.at = data, time.Now()
	return c.body, nil
}
