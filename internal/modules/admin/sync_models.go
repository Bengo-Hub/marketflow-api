package admin

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/platform/modelcatalog"
	"go.uber.org/zap"
)

// SyncModelsRequest is the POST body for /admin/providers/sync-models.
type SyncModelsRequest struct {
	ProviderType string `json:"provider_type"`
	ProviderName string `json:"provider_name"`
	Environment  string `json:"environment"`
}

// ModelEntry is a single row in the sync response.
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// SyncModels fetches the provider's live model list with the platform credentials and recommends
// a model for every role the provider fills (modelcatalog): router, responder and fallback for
// Groq; standard and premium for OpenAI; Haiku and Sonnet for Anthropic; local chat and
// embeddings for Ollama; standard and premium voice for ElevenLabs; and, for information, the
// newest fal model per media tier. The portal preselects the recommendations; nothing is saved
// until the owner saves the form.
//
// @Summary Sync AI models
// @Description Lists the provider's live models with the stored key and recommends the newest stable model for each role it fills.
// @Tags Admin - Providers
// @Accept json
// @Produce json
// @Param request body SyncModelsRequest true "Which provider to sync"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Failure 502 {object} map[string]string
// @Security bearerAuth
// @Router /admin/providers/sync-models [post]
func (h *PlatformProviderHandler) SyncModels(w http.ResponseWriter, r *http.Request) {
	var req SyncModelsRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.ProviderType == "" || req.ProviderName == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_params", "provider_type and provider_name required", nil)
		return
	}
	if req.Environment == "" {
		req.Environment = "production"
	}
	if _, ok := modelcatalog.Sources[req.ProviderName]; !ok {
		httpapi.Error(w, http.StatusBadRequest, "not_supported", req.ProviderName+" does not publish a model list", nil)
		return
	}

	settings, err := h.loadSettings(r.Context(), req.ProviderType, req.ProviderName, req.Environment)
	if err != nil && !modelcatalog.Keyless(req.ProviderName) {
		httpapi.Error(w, http.StatusBadRequest, "not_configured", err.Error(), nil)
		return
	}
	apiKey := strings.TrimSpace(settings["api_key"])
	if apiKey == "" && !modelcatalog.Keyless(req.ProviderName) {
		httpapi.Error(w, http.StatusBadRequest, "missing_api_key", "no api_key configured for this provider", nil)
		return
	}

	ids, err := modelcatalog.List(r.Context(), &http.Client{Timeout: 15 * time.Second}, req.ProviderName, apiKey, settings["base_url"])
	if err != nil {
		h.logger.Warn("sync-models failed", zap.String("provider", req.ProviderName), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "sync_failed", err.Error(), nil)
		return
	}
	sort.Strings(ids)
	models := make([]ModelEntry, len(ids))
	for i, id := range ids {
		models[i] = ModelEntry{ID: id, Object: "model", OwnedBy: req.ProviderName}
	}
	recs := modelcatalog.Recommend(ids, modelcatalog.RolesFor(req.ProviderName, h.roleOverrides(r.Context())), settings)
	recommended := map[string]string{}
	for _, rec := range recs {
		if !rec.Informational {
			recommended[rec.Field] = rec.Model
		}
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"provider_type":   req.ProviderType,
		"provider_name":   req.ProviderName,
		"count":           len(models),
		"models":          models,
		"recommended":     recommended,
		"recommendations": recs,
	})
}

// roleOverrides reads the platform's role families (model_watch/roles, key "roles").
func (h *PlatformProviderHandler) roleOverrides(ctx context.Context) string {
	s, err := h.loadSettings(ctx, "model_watch", "roles", "production")
	if err != nil {
		return ""
	}
	return s["roles"]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
