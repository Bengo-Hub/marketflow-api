package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"go.uber.org/zap"
)

// TestProviderRequest is the POST body for /admin/providers/test.
type TestProviderRequest struct {
	ProviderType string `json:"provider_type"`
	ProviderName string `json:"provider_name"`
	Environment  string `json:"environment"`
}

// TestProviderResponse reports whether credentials actually work against the
// upstream provider. The probe is kept to the cheapest possible call for each
// provider (one-token chat, /me, OAuth token refresh, etc).
type TestProviderResponse struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Message   string `json:"message,omitempty"`
}

// TestProvider verifies that the platform-configured credentials for a given
// provider actually work. Each provider_name maps to a specific probe.
//
// @Summary Test a platform provider
// @Description Sends a minimal, provider-specific probe (1-token LLM chat, /me, OAuth refresh, etc) to verify the stored credentials work. Safe to call repeatedly; no state change.
// @Tags Admin - Providers
// @Accept json
// @Produce json
// @Param request body TestProviderRequest true "Which provider to test"
// @Success 200 {object} TestProviderResponse
// @Failure 400 {object} map[string]string
// @Failure 502 {object} map[string]string
// @Security bearerAuth
// @Router /admin/providers/test [post]
func (h *PlatformProviderHandler) TestProvider(w http.ResponseWriter, r *http.Request) {
	var req TestProviderRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.ProviderType == "" || req.ProviderName == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_params",
			"provider_type and provider_name required", nil)
		return
	}
	if req.Environment == "" {
		req.Environment = "production"
	}

	settings, err := h.loadSettings(r.Context(), req.ProviderType, req.ProviderName, req.Environment)
	if err != nil {
		httpapi.JSON(w, http.StatusOK, TestProviderResponse{OK: false, Message: err.Error()})
		return
	}

	started := time.Now()
	msg, ok := runProbe(r.Context(), req.ProviderType, req.ProviderName, settings)
	latency := time.Since(started).Milliseconds()

	h.logger.Info("provider test",
		zap.String("type", req.ProviderType),
		zap.String("name", req.ProviderName),
		zap.Bool("ok", ok),
		zap.Int64("latency_ms", latency),
	)
	httpapi.JSON(w, http.StatusOK, TestProviderResponse{OK: ok, LatencyMs: latency, Message: msg})
}

func (h *PlatformProviderHandler) loadSettings(ctx context.Context, ptype, pname, env string) (map[string]string, error) {
	rows, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(platformTenantID),
			providersetting.ProviderTypeEQ(ptype),
			providersetting.ProviderNameEQ(pname),
			providersetting.EnvironmentEQ(env),
			providersetting.IsActiveEQ(true),
		).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no active settings for %s/%s — save a config first", ptype, pname)
	}
	out := make(map[string]string, len(rows))
	for _, s := range rows {
		v := s.Value
		if s.IsEncrypted && len(h.encryptionKey) > 0 {
			if dec, err := encryption.Decrypt(v, h.encryptionKey); err == nil {
				v = dec
			}
		}
		out[s.Key] = v
	}
	return out, nil
}

// runProbe dispatches on provider_name. Returns (message, ok). A failing probe
// still returns OK=false with a human-readable reason — never a 5xx from us.
func runProbe(ctx context.Context, ptype, pname string, s map[string]string) (string, bool) {
	c := &http.Client{Timeout: 10 * time.Second}
	switch pname {
	case "groq":
		return probeOpenAIChat(ctx, c, firstNonEmpty(s["base_url"], "https://api.groq.com/openai/v1"),
			s["api_key"], firstNonEmpty(s["router_model"], "llama-3.1-8b-instant"))
	case "openai":
		return probeOpenAIChat(ctx, c, firstNonEmpty(s["base_url"], "https://api.openai.com/v1"),
			s["api_key"], firstNonEmpty(s["main_model"], "gpt-4o-mini"))
	case "anthropic":
		return probeAnthropicChat(ctx, c, firstNonEmpty(s["base_url"], "https://api.anthropic.com/v1"),
			s["api_key"], firstNonEmpty(s["main_model"], "claude-haiku-4-5"))
	case "ollama":
		return probeOllama(ctx, c, firstNonEmpty(s["base_url"], "http://ollama.shared-infra.svc.cluster.local:11434"),
			firstNonEmpty(s["embedding_model"], "nomic-embed-text"))
	case "tavily":
		return probeTavily(ctx, c, firstNonEmpty(s["base_url"], "https://api.tavily.com"), s["api_key"])
	case "custom_search":
		return probeGoogleCSE(ctx, c, s["api_key"], s["cse_id"])
	case "default": // cal.com
		if ptype == "calcom" {
			return probeCalcom(ctx, c, firstNonEmpty(s["base_url"], "https://api.cal.com/v2"), s["api_key"])
		}
	}
	// Ad / OAuth-first providers: we can only validate the static app-level bits.
	switch ptype {
	case "meta_ads":
		return probeMetaApp(ctx, c, s["app_id"], s["app_secret"])
	case "google_ads", "google_calendar":
		return probeGoogleOAuthClient(ctx, c, s["client_id"])
	case "linkedin_ads", "tiktok_ads", "x_ads":
		if s["client_id"] != "" || s["app_id"] != "" || s["consumer_key"] != "" {
			return "Client credentials present — full OAuth test runs on tenant connect", true
		}
		return "missing client credentials", false
	}
	return fmt.Sprintf("no test probe implemented for %s/%s", ptype, pname), false
}

// ── Probes ───────────────────────────────────────────────────────────────

func probeOpenAIChat(ctx context.Context, c *http.Client, baseURL, key, model string) (string, bool) {
	if key == "" {
		return "api_key missing", false
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with only: ok"}},
		"max_tokens": 3,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("%s: %s", resp.Status, truncate(string(b), 160)), false
	}
	return "Chat 1-token probe succeeded", true
}

func probeAnthropicChat(ctx context.Context, c *http.Client, baseURL, key, model string) (string, bool) {
	if key == "" {
		return "api_key missing", false
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 4,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with only: ok"}},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(baseURL, "/")+"/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("%s: %s", resp.Status, truncate(string(b), 160)), false
	}
	return "Messages 1-token probe succeeded", true
}

func probeOllama(ctx context.Context, c *http.Client, baseURL, model string) (string, bool) {
	body, _ := json.Marshal(map[string]any{"model": model, "prompt": "hello"})
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(baseURL, "/")+"/api/embeddings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("%s: %s", resp.Status, truncate(string(b), 160)), false
	}
	var parsed struct {
		Embedding []float64 `json:"embedding"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &parsed); err != nil || len(parsed.Embedding) == 0 {
		return "empty embedding returned", false
	}
	return fmt.Sprintf("Embedding OK (%d dims)", len(parsed.Embedding)), true
}

func probeTavily(ctx context.Context, c *http.Client, baseURL, key string) (string, bool) {
	if key == "" {
		return "api_key missing", false
	}
	body, _ := json.Marshal(map[string]any{
		"api_key": key, "query": "ping", "max_results": 1, "search_depth": "basic",
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(baseURL, "/")+"/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("%s: %s", resp.Status, truncate(string(b), 160)), false
	}
	return "Search probe succeeded", true
}

func probeGoogleCSE(ctx context.Context, c *http.Client, key, cseID string) (string, bool) {
	if key == "" || cseID == "" {
		return "api_key or cse_id missing", false
	}
	u := fmt.Sprintf("https://www.googleapis.com/customsearch/v1?key=%s&cx=%s&q=ping&num=1",
		url.QueryEscape(key), url.QueryEscape(cseID))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("%s: %s", resp.Status, truncate(string(b), 160)), false
	}
	return "Search probe succeeded", true
}

func probeCalcom(ctx context.Context, c *http.Client, baseURL, key string) (string, bool) {
	if key == "" {
		return "api_key missing", false
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(baseURL, "/")+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("cal-api-version", "2024-08-13")
	resp, err := c.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("%s: %s", resp.Status, truncate(string(b), 160)), false
	}
	return "/me probe succeeded", true
}

// probeMetaApp validates app_id + app_secret by requesting an app access token.
func probeMetaApp(ctx context.Context, c *http.Client, appID, appSecret string) (string, bool) {
	if appID == "" || appSecret == "" {
		return "app_id or app_secret missing", false
	}
	u := fmt.Sprintf("https://graph.facebook.com/oauth/access_token?client_id=%s&client_secret=%s&grant_type=client_credentials",
		url.QueryEscape(appID), url.QueryEscape(appSecret))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("%s: %s", resp.Status, truncate(string(b), 160)), false
	}
	return "App access token issued", true
}

// probeGoogleOAuthClient only verifies the client_id format; the real OAuth
// flow runs when a tenant clicks "Connect".
func probeGoogleOAuthClient(_ context.Context, _ *http.Client, clientID string) (string, bool) {
	if clientID == "" {
		return "client_id missing", false
	}
	if !strings.HasSuffix(clientID, ".apps.googleusercontent.com") {
		return "client_id should end with .apps.googleusercontent.com", false
	}
	return "Client ID format valid — full OAuth test runs on tenant connect", true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
