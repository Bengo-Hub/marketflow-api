// Package modelcatalog lists each AI provider's live models and recommends one model for every
// role a provider fills (router, responder, fallback, premium, voice and so on). The admin
// "Sync models" action and the daily model watch both read the live lists through it, so the
// two never disagree about what a provider offers.
//
// Model ids are never fixed in code: a role names model families, and the newest stable model of
// the first family the provider lists wins. The families below are the documented fallback list;
// the platform replaces any of them with ProviderSetting model_watch/roles (key "roles", JSON
// {"anthropic": {"plus_model": {"families": ["claude-opus-"]}}}).
package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Source is one provider's model listing.
type Source struct {
	Provider string
	// Type and Name are the platform ProviderSetting the key is stored under.
	Type, Name string
	// DefaultBase is the API base when the setting has no base_url.
	DefaultBase string
	// Path is appended to the base; Query is added to the first page.
	Path, Query string
	Auth        func(req *http.Request, key string)
	Parse       func([]byte) (ids []string, next string)
	// Describe, when set, replaces Parse and also reads each model's age, price and inputs.
	Describe func([]byte) (models []Model, next string)
	// Page builds the next page's URL from the first page's URL and the cursor.
	Page func(first, next string) string
}

// Model is one listed model. Lists that only give ids leave the other fields zero.
type Model struct {
	ID string `json:"id"`
	// Created is the release time in Unix seconds, when the list gives one.
	Created int64 `json:"created,omitempty"`
	// Free is true when the provider charges nothing for it (OpenRouter's :free models).
	Free bool `json:"free,omitempty"`
	// Vision is true when it accepts images, so it can serve the media review.
	Vision bool `json:"vision,omitempty"`
}

func bearer(req *http.Request, key string) { req.Header.Set("Authorization", "Bearer "+key) }

// Sources are the providers whose model lists can be read.
var Sources = map[string]Source{
	"groq":   {Provider: "groq", Type: "ai_llm", Name: "groq", DefaultBase: "https://api.groq.com/openai/v1", Path: "/models", Auth: bearer, Parse: ParseOpenAI},
	"openai": {Provider: "openai", Type: "ai_llm", Name: "openai", DefaultBase: "https://api.openai.com/v1", Path: "/models", Auth: bearer, Parse: ParseOpenAI},
	"anthropic": {Provider: "anthropic", Type: "ai_llm", Name: "anthropic", DefaultBase: "https://api.anthropic.com/v1", Path: "/models", Query: "limit=1000",
		Auth: func(req *http.Request, key string) {
			req.Header.Set("x-api-key", key)
			req.Header.Set("anthropic-version", "2023-06-01")
		}, Parse: ParseAnthropic,
		Page: func(first, next string) string { return first + "&after_id=" + url.QueryEscape(next) }},
	"ollama": {Provider: "ollama", Type: "ai_embed", Name: "ollama", DefaultBase: "http://ollama.shared-infra.svc.cluster.local:11434", Path: "/api/tags",
		Auth: func(*http.Request, string) {}, Parse: ParseOllama},
	"elevenlabs": {Provider: "elevenlabs", Type: "media", Name: "elevenlabs", DefaultBase: "https://api.elevenlabs.io/v1", Path: "/models",
		Auth: func(req *http.Request, key string) { req.Header.Set("xi-api-key", key) }, Parse: ParseElevenLabs},
	"fal": {Provider: "fal", Type: "media", Name: "fal", DefaultBase: "https://api.fal.ai/v1", Path: "/models", Query: "limit=100",
		Auth:  func(req *http.Request, key string) { req.Header.Set("Authorization", "Key "+key) },
		Parse: ParseFal,
		Page:  func(first, next string) string { return first + "&cursor=" + url.QueryEscape(next) }},
	// The free-tier fallbacks use OpenAI-compatible endpoints: GET {base_url}/models with the
	// saved key. OpenRouter's list also carries prices and input modalities.
	"openrouter": {Provider: "openrouter", Type: "ai_llm", Name: "openrouter", DefaultBase: "https://openrouter.ai/api/v1", Path: "/models",
		Auth: bearer, Describe: DescribeOpenRouter},
	"gemini": {Provider: "gemini", Type: "ai_llm", Name: "gemini", DefaultBase: "https://generativelanguage.googleapis.com/v1beta/openai", Path: "/models",
		Auth: bearer, Describe: DescribeGemini},
	"cerebras": {Provider: "cerebras", Type: "ai_llm", Name: "cerebras", DefaultBase: "https://api.cerebras.ai/v1", Path: "/models",
		Auth: bearer, Describe: DescribeOpenAI},
}

// Keyless reports whether a provider lists its models without an API key (the cluster's Ollama).
func Keyless(provider string) bool { return provider == "ollama" }

// List reads every page of a provider's model list and returns the ids.
func List(ctx context.Context, hc *http.Client, provider, key, baseURL string) ([]string, error) {
	models, err := ListModels(ctx, hc, provider, key, baseURL)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids, nil
}

// ListModels reads every page of a provider's model list.
func ListModels(ctx context.Context, hc *http.Client, provider, key, baseURL string) ([]Model, error) {
	src, ok := Sources[provider]
	if !ok {
		return nil, fmt.Errorf("%s does not publish a model list", provider)
	}
	base := strings.TrimSuffix(firstNonEmpty(baseURL, src.DefaultBase), "/")
	first := base + src.Path
	if src.Query != "" {
		first += "?" + src.Query
	}
	var all []Model
	next := ""
	for page := 0; page < 30; page++ {
		u := first
		if next != "" && src.Page != nil {
			u = src.Page(first, next)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		src.Auth(req, key)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s models: status %d: %s", provider, resp.StatusCode, truncate(string(body), 200))
		}
		var n string
		if src.Describe != nil {
			var ms []Model
			ms, n = src.Describe(body)
			all = append(all, ms...)
		} else {
			var ids []string
			ids, n = src.Parse(body)
			for _, id := range ids {
				all = append(all, Model{ID: id})
			}
		}
		if n == "" || src.Page == nil {
			break
		}
		next = n
	}
	return all, nil
}

// ParseOpenAI reads an OpenAI-compatible list ({data: [{id}]}).
func ParseOpenAI(b []byte) ([]string, string) {
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil, ""
	}
	var out []string
	for _, m := range v.Data {
		out = append(out, m.ID)
	}
	return out, ""
}

// DescribeOpenAI reads an OpenAI-compatible list with each model's created time (Cerebras:
// {object: "list", data: [{id, object, created, owned_by}]}).
func DescribeOpenAI(b []byte) ([]Model, string) {
	var v struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil, ""
	}
	var out []Model
	for _, m := range v.Data {
		if m.ID != "" {
			out = append(out, Model{ID: m.ID, Created: m.Created})
		}
	}
	return out, ""
}

// DescribeGemini reads Gemini's OpenAI-compatible list. Ids come back as "models/gemini-...";
// chat completions take the bare id, so the prefix is dropped.
func DescribeGemini(b []byte) ([]Model, string) {
	models, next := DescribeOpenAI(b)
	for i := range models {
		models[i].ID = strings.TrimPrefix(models[i].ID, "models/")
		// Every current Gemini chat model reads images.
		models[i].Vision = strings.HasPrefix(models[i].ID, "gemini-") && !strings.Contains(models[i].ID, "embedding")
	}
	return models, next
}

// DescribeOpenRouter reads OpenRouter's /models: {data: [{id, created, pricing: {prompt,
// completion}, architecture: {input_modalities}}]}. Prices are decimal strings in USD per
// token; a model is free when both are zero or its id ends in ":free". A negative price
// (openrouter/auto) means variable, never free.
func DescribeOpenRouter(b []byte) ([]Model, string) {
	var v struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			Architecture struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil, ""
	}
	var out []Model
	for _, m := range v.Data {
		if m.ID == "" {
			continue
		}
		vision := false
		for _, in := range m.Architecture.InputModalities {
			if strings.EqualFold(in, "image") {
				vision = true
			}
		}
		free := strings.HasSuffix(m.ID, ":free") || (zeroPrice(m.Pricing.Prompt) && zeroPrice(m.Pricing.Completion))
		out = append(out, Model{ID: m.ID, Created: m.Created, Free: free, Vision: vision})
	}
	return out, ""
}

func zeroPrice(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

// ParseAnthropic reads Anthropic's paged list.
func ParseAnthropic(b []byte) ([]string, string) {
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil, ""
	}
	var out []string
	for _, m := range v.Data {
		out = append(out, m.ID)
	}
	if !v.HasMore {
		return out, ""
	}
	return out, v.LastID
}

// ParseOllama reads Ollama's /api/tags.
func ParseOllama(b []byte) ([]string, string) {
	var v struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil, ""
	}
	var out []string
	for _, m := range v.Models {
		out = append(out, m.Name)
	}
	return out, ""
}

// ParseElevenLabs reads ElevenLabs' model array.
func ParseElevenLabs(b []byte) ([]string, string) {
	var v []struct {
		ModelID string `json:"model_id"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil, ""
	}
	var out []string
	for _, m := range v {
		out = append(out, m.ModelID)
	}
	return out, ""
}

// ParseFal reads fal's paged model catalogue.
func ParseFal(b []byte) ([]string, string) {
	var v struct {
		Models []struct {
			EndpointID string `json:"endpoint_id"`
			ID         string `json:"id"`
		} `json:"models"`
		HasMore    bool   `json:"has_more"`
		NextCursor string `json:"next_cursor"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil, ""
	}
	var out []string
	for _, m := range v.Models {
		if id := firstNonEmpty(m.EndpointID, m.ID); id != "" {
			out = append(out, id)
		}
	}
	if !v.HasMore {
		return out, ""
	}
	return out, v.NextCursor
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
