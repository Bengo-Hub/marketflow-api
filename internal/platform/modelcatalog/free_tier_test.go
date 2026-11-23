package modelcatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serve answers GET /models with body when the bearer key is "k".
func serve(t *testing.T, path, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pickFor(t *testing.T, provider string, models []Model) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range RecommendModels(models, RolesFor(provider, ""), nil) {
		out[r.Field] = r.Model
	}
	return out
}

func TestOpenRouterPicksTheNewestFreeVisionModel(t *testing.T) {
	srv := serve(t, "/api/v1/models", `{"data":[
		{"id":"openrouter/auto","created":1800000000,"pricing":{"prompt":"-1","completion":"-1"},"architecture":{"input_modalities":["text","image"]}},
		{"id":"anthropic/claude-sonnet-5","created":1790000000,"pricing":{"prompt":"0.000003","completion":"0.000015"},"architecture":{"input_modalities":["text","image"]}},
		{"id":"meta-llama/llama-3.3-70b-instruct:free","created":1733000000,"pricing":{"prompt":"0","completion":"0"},"architecture":{"input_modalities":["text"]}},
		{"id":"deepseek/deepseek-r2:free","created":1780000000,"pricing":{"prompt":"0","completion":"0"},"architecture":{"input_modalities":["text"]}},
		{"id":"qwen/qwen3-vl-235b-a22b-instruct:free","created":1760000000,"pricing":{"prompt":"0","completion":"0"},"architecture":{"input_modalities":["text","image"]}},
		{"id":"google/gemma-3-27b-it:free","created":1741000000,"pricing":{"prompt":"0","completion":"0"},"architecture":{"input_modalities":["image","text"]}},
		{"id":"mistralai/mistral-small-preview:free","created":1799000000,"pricing":{"prompt":"0","completion":"0"},"architecture":{"input_modalities":["text","image"]}}
	]}`)
	models, err := ListModels(context.Background(), srv.Client(), "openrouter", "k", srv.URL+"/api/v1")
	if err != nil || len(models) != 7 {
		t.Fatalf("models %v err %v", models, err)
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if byID["openrouter/auto"].Free || byID["anthropic/claude-sonnet-5"].Free || !byID["deepseek/deepseek-r2:free"].Free {
		t.Fatalf("free flags wrong: %+v", models)
	}
	if !byID["google/gemma-3-27b-it:free"].Vision || byID["deepseek/deepseek-r2:free"].Vision {
		t.Fatalf("vision flags wrong: %+v", models)
	}
	got := pickFor(t, "openrouter", models)
	// The newest free text model is deepseek, but main_model prefers a vision model; the preview
	// is skipped as unstable.
	if got["main_model"] != "qwen/qwen3-vl-235b-a22b-instruct:free" {
		t.Fatalf("main_model = %q", got["main_model"])
	}
	if got["premium_model"] != "qwen/qwen3-vl-235b-a22b-instruct:free" {
		t.Fatalf("premium_model = %q", got["premium_model"])
	}

	// Without any free vision model the newest free model wins.
	text := pickFor(t, "openrouter", []Model{
		{ID: "meta-llama/llama-3.3-70b-instruct:free", Created: 1733000000, Free: true},
		{ID: "deepseek/deepseek-r2:free", Created: 1780000000, Free: true},
		{ID: "openai/gpt-5", Created: 1790000000, Vision: true},
	})
	if text["main_model"] != "deepseek/deepseek-r2:free" || text["premium_model"] != "meta-llama/llama-3.3-70b-instruct:free" {
		t.Fatalf("text only = %v", text)
	}
}

func TestGeminiPicksTheNewestStableFlash(t *testing.T) {
	srv := serve(t, "/v1beta/openai/models", `{"object":"list","data":[
		{"id":"models/gemini-2.0-flash","object":"model","owned_by":"google"},
		{"id":"models/gemini-2.5-flash","object":"model","owned_by":"google"},
		{"id":"models/gemini-2.5-flash-lite","object":"model","owned_by":"google"},
		{"id":"models/gemini-3.0-flash-preview","object":"model","owned_by":"google"},
		{"id":"models/gemini-2.5-pro","object":"model","owned_by":"google"},
		{"id":"models/gemini-flash-latest","object":"model","owned_by":"google"},
		{"id":"models/gemini-embedding-001","object":"model","owned_by":"google"}
	]}`)
	// The catalog's default base ends in a slash.
	models, err := ListModels(context.Background(), srv.Client(), "gemini", "k", srv.URL+"/v1beta/openai/")
	if err != nil || len(models) != 7 {
		t.Fatalf("models %v err %v", models, err)
	}
	if models[0].ID != "gemini-2.0-flash" || !models[0].Vision || models[6].Vision {
		t.Fatalf("ids keep no prefix and only chat models read images: %+v", models)
	}
	if got := pickFor(t, "gemini", models); got["main_model"] != "gemini-2.5-flash" {
		t.Fatalf("gemini = %v", got)
	}
}

func TestCerebrasPicksTheLargestGeneralModel(t *testing.T) {
	srv := serve(t, "/v1/models", `{"object":"list","data":[
		{"id":"llama3.1-8b","object":"model","created":0,"owned_by":"Meta"},
		{"id":"llama-3.3-70b","object":"model","created":0,"owned_by":"Meta"},
		{"id":"gpt-oss-120b","object":"model","created":0,"owned_by":"OpenAI"},
		{"id":"qwen-3-32b","object":"model","created":0,"owned_by":"Qwen"},
		{"id":"zai-glm-4.6","object":"model","created":0,"owned_by":"Z.ai"},
		{"id":"qwen-3-480b-coder-preview","object":"model","created":0,"owned_by":"Qwen"}
	]}`)
	models, err := ListModels(context.Background(), srv.Client(), "cerebras", "k", srv.URL+"/v1")
	if err != nil || len(models) != 6 {
		t.Fatalf("models %v err %v", models, err)
	}
	if got := pickFor(t, "cerebras", models); got["main_model"] != "gpt-oss-120b" {
		t.Fatalf("cerebras = %v", got)
	}
	if _, err := ListModels(context.Background(), srv.Client(), "cerebras", "bad", srv.URL+"/v1"); err == nil {
		t.Fatal("a refused key must fail")
	}
}

func TestSizeOf(t *testing.T) {
	for id, want := range map[string]float64{"gpt-oss-120b": 120, "qwen-3-235b-a22b-instruct-2507": 235, "llama3.1-8b": 8, "zai-glm-4.6": 0, "smol-360m": 0.36} {
		if got := sizeOf(id); got != want {
			t.Errorf("sizeOf(%q) = %v, want %v", id, got, want)
		}
	}
}
