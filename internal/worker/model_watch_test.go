package worker

import (
	"reflect"
	"sort"
	"testing"
)

// The model watch (owner, 2026-11-11) reads each provider's live model list, keeps only the
// families MarketFlow uses, and reports models it has not seen before.
func TestParseModelLists(t *testing.T) {
	cases := map[string]struct {
		parse func([]byte) ([]string, string)
		body  string
		ids   []string
		next  string
	}{
		"openai-style": {parseOpenAIModels, `{"data":[{"id":"llama-4"},{"id":"qwen-3"}]}`, []string{"llama-4", "qwen-3"}, ""},
		"anthropic":    {parseAnthropicModels, `{"data":[{"id":"claude-haiku-5-5"}],"has_more":true,"last_id":"claude-haiku-5-5"}`, []string{"claude-haiku-5-5"}, "claude-haiku-5-5"},
		"elevenlabs":   {parseElevenLabsModels, `[{"model_id":"eleven_v4_turbo"},{"model_id":"eleven_v5"}]`, []string{"eleven_v4_turbo", "eleven_v5"}, ""},
		"fal":          {parseFalModels, `{"models":[{"endpoint_id":"creatify/boreal"},{"id":"fal-ai/kling-video/v4"}],"has_more":true,"next_cursor":"c2"}`, []string{"creatify/boreal", "fal-ai/kling-video/v4"}, "c2"},
		"broken":       {parseOpenAIModels, `nope`, nil, ""},
	}
	for name, c := range cases {
		ids, next := c.parse([]byte(c.body))
		if !reflect.DeepEqual(ids, c.ids) || next != c.next {
			t.Errorf("%s: got %v %q, want %v %q", name, ids, next, c.ids, c.next)
		}
	}
}

func TestNewWatchedModels(t *testing.T) {
	families := []string{"kling", "boreal"}
	live := []string{"fal-ai/kling-video/v4", "creatify/boreal", "fal-ai/flux/dev", "fal-ai/kling-video/v3"}
	seen := map[string]bool{"fal-ai/kling-video/v3": true, "creatify/boreal": true}
	got := newWatchedModels(live, seen, families)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"fal-ai/kling-video/v4"}) {
		t.Fatalf("new watched models: %v", got)
	}
	if got := watched(live, families); len(got) != 3 {
		t.Fatalf("watched: %v", got)
	}
}
