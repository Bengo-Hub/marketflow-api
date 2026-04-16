package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
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

// SyncModels fetches the live list of available models from an AI provider
// using the platform-configured credentials. Supports Groq, OpenAI, Anthropic.
//
// @Summary Sync AI models
// @Description Calls the provider's /models endpoint with the stored API key and returns the available model IDs so the platform admin can pick one from a dropdown.
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

	// Load + decrypt settings.
	rows, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(platformTenantID),
			providersetting.ProviderTypeEQ(req.ProviderType),
			providersetting.ProviderNameEQ(req.ProviderName),
			providersetting.EnvironmentEQ(req.Environment),
			providersetting.IsActiveEQ(true),
		).All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	settings := make(map[string]string, len(rows))
	for _, s := range rows {
		v := s.Value
		if s.IsEncrypted && len(h.encryptionKey) > 0 {
			if dec, err := encryption.Decrypt(v, h.encryptionKey); err == nil {
				v = dec
			}
		}
		settings[s.Key] = v
	}
	apiKey := settings["api_key"]
	if apiKey == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_api_key",
			"no api_key configured for this provider", nil)
		return
	}

	models, err := fetchModels(r.Context(), req.ProviderName, apiKey, settings["base_url"])
	if err != nil {
		h.logger.Warn("sync-models failed",
			zap.String("provider", req.ProviderName), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "sync_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"provider_type": req.ProviderType,
		"provider_name": req.ProviderName,
		"count":         len(models),
		"models":        models,
	})
}

// fetchModels calls the provider's model-listing endpoint. Shape:
//   - Groq / OpenAI:  GET {base_url}/models  (OpenAI-compatible)
//     → { data: [{id, object, owned_by}] }
//   - Anthropic:      GET {base_url}/models  (requires x-api-key + anthropic-version)
//     → { data: [{id, display_name, type}] }
func fetchModels(ctx context.Context, providerName, apiKey, baseURL string) ([]ModelEntry, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	switch providerName {
	case "groq", "openai":
		if baseURL == "" {
			if providerName == "groq" {
				baseURL = "https://api.groq.com/openai/v1"
			} else {
				baseURL = "https://api.openai.com/v1"
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/models", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		return doOpenAICompat(client, req)
	case "anthropic":
		if baseURL == "" {
			baseURL = "https://api.anthropic.com/v1"
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/models", nil)
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return doAnthropic(client, req)
	default:
		return nil, fmt.Errorf("sync-models not supported for provider %q", providerName)
	}
}

func doOpenAICompat(c *http.Client, req *http.Request) ([]ModelEntry, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("provider %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var parsed struct {
		Data []ModelEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return parsed.Data, nil
}

func doAnthropic(c *http.Client, req *http.Request) ([]ModelEntry, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var parsed struct {
		Data []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	out := make([]ModelEntry, len(parsed.Data))
	for i, m := range parsed.Data {
		out[i] = ModelEntry{ID: m.ID, Object: m.Type, OwnedBy: "anthropic"}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
