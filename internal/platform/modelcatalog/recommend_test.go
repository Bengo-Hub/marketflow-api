package modelcatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func recs(t *testing.T, provider string, ids []string, current map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range Recommend(ids, RolesFor(provider, ""), current) {
		out[r.Field] = r.Model
	}
	return out
}

// Every role gets the newest stable model of its family, not just the premium one.
func TestRecommendPicksTheNewestStableModelPerRole(t *testing.T) {
	openai := recs(t, "openai", []string{"gpt-4o-mini", "gpt-4o-mini-2024-07-18", "gpt-4.1-mini", "gpt-5-mini", "gpt-5-mini-2025-08-07",
		"gpt-5.1", "gpt-5", "gpt-5-chat-latest", "gpt-5.2-preview", "gpt-4o", "gpt-5-nano", "gpt-4o-realtime-preview", "gpt-3.5-turbo-instruct"}, nil)
	if openai["main_model"] != "gpt-5-mini" || openai["premium_model"] != "gpt-5.1" {
		t.Fatalf("openai = %v", openai)
	}

	claude := recs(t, "anthropic", []string{"claude-3-5-haiku-20241022", "claude-haiku-4-5-20251001", "claude-haiku-5-5",
		"claude-sonnet-4-5-20250929", "claude-sonnet-5-5", "claude-opus-4-1-20250805"}, nil)
	if claude["main_model"] != "claude-haiku-5-5" || claude["plus_model"] != "claude-sonnet-5-5" {
		t.Fatalf("anthropic = %v", claude)
	}

	groq := recs(t, "groq", []string{"openai/gpt-oss-20b", "openai/gpt-oss-120b", "openai/gpt-oss-safeguard-20b",
		"llama-3.1-8b-instant", "llama-3.3-70b-versatile", "whisper-large-v3"}, nil)
	if groq["router_model"] != "openai/gpt-oss-20b" || groq["main_model"] != "openai/gpt-oss-120b" || groq["fallback_model"] != "openai/gpt-oss-20b" {
		t.Fatalf("groq = %v", groq)
	}

	voice := recs(t, "elevenlabs", []string{"eleven_v4_turbo", "eleven_v3", "eleven_v4", "eleven_turbo_v2_5", "eleven_multilingual_v2", "eleven_multilingual_sts_v2"}, nil)
	if voice["voice_model"] != "eleven_v4_turbo" || voice["voice_premium_model"] != "eleven_v4" {
		t.Fatalf("elevenlabs = %v", voice)
	}

	fal := Recommend([]string{"fal-ai/bytedance/seedream/v4/text-to-image", "fal-ai/bytedance/seedream/v5/lite/text-to-image",
		"fal-ai/minimax/hailuo-2.3-fast/standard/image-to-video", "fal-ai/minimax/hailuo-02/standard/image-to-video"}, RolesFor("fal", ""), nil)
	got := map[string]Recommendation{}
	for _, r := range fal {
		got[r.Field] = r
	}
	if got["image_standard"].Model != "fal-ai/bytedance/seedream/v5/lite/text-to-image" || !got["image_standard"].Informational {
		t.Fatalf("fal image = %+v", got["image_standard"])
	}
	if got["video_standard"].Model != "fal-ai/minimax/hailuo-2.3-fast/standard/image-to-video" {
		t.Fatalf("fal video = %+v", got["video_standard"])
	}
}

// Ollama keeps an installed model; the platform's overrides replace or drop a role.
func TestRecommendKeepsCurrentAndHonoursOverrides(t *testing.T) {
	ids := []string{"llama3.1:8b", "llama3.2:3b", "nomic-embed-text:latest", "nomic-embed-text"}
	got := recs(t, "ollama", ids, map[string]string{"fallback_model": "llama3.1:8b"})
	if got["fallback_model"] != "llama3.1:8b" || got["embedding_model"] != "nomic-embed-text" {
		t.Fatalf("ollama = %v", got)
	}

	roles := RolesFor("anthropic", `{"anthropic":{"plus_model":{"label":"Opus","families":["opus"]},"main_model":{"families":[]}}}`)
	if _, ok := roles["main_model"]; ok {
		t.Fatal("an override with no families drops the role")
	}
	r := Recommend([]string{"claude-opus-4-1-20250805", "claude-opus-5"}, roles, nil)
	if len(r) != 1 || r[0].Model != "claude-opus-5" {
		t.Fatalf("override = %+v", r)
	}
	if len(RolesFor("anthropic", "{bad")) != 2 {
		t.Fatal("bad JSON keeps the defaults")
	}
}

func TestListFollowsPages(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("x-api-key") != "k" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-haiku-5-5"}],"has_more":true,"last_id":"claude-haiku-5-5"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5-5"}],"has_more":false}`))
	}))
	defer srv.Close()
	ids, err := List(context.Background(), srv.Client(), "anthropic", "k", srv.URL)
	if err != nil || len(ids) != 2 || calls != 2 {
		t.Fatalf("ids %v err %v calls %d", ids, err, calls)
	}
	if _, err := List(context.Background(), srv.Client(), "anthropic", "bad", srv.URL); err == nil {
		t.Fatal("a refused key must fail")
	}
	if _, err := List(context.Background(), srv.Client(), "higgsfield", "k", ""); err == nil {
		t.Fatal("a provider without a model list must say so")
	}
}
