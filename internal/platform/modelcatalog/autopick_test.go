package modelcatalog

import "testing"

// Sync models moves a route to a newer version of its own family and tier only: never to another
// family or variant, never to a preview, alpha or beta build, and never down.
func TestNewerInFamily(t *testing.T) {
	m := func(id, cat string) Model { return Model{ID: id, Category: cat} }
	eleven := []Model{m("eleven_v4", ""), m("eleven_v4_turbo", ""), m("eleven_v5", ""), m("eleven_v5_turbo", ""),
		m("eleven_v6_alpha", ""), m("eleven_multilingual_v3", ""), m("eleven_v3", "")}
	fal := []Model{
		m("elevenlabs/tts/eleven-v4", "text-to-speech"), m("elevenlabs/tts/eleven-v4-turbo", "text-to-speech"),
		m("elevenlabs/tts/eleven-v5-turbo", "text-to-speech"), m("elevenlabs/tts/eleven-v6-preview", "text-to-speech"),
		m("fal-ai/kling-video/v3/pro/image-to-video", "image-to-video"), m("fal-ai/kling-video/v3.5/pro/image-to-video", "image-to-video"),
		m("fal-ai/kling-video/v4/pro/text-to-video", "text-to-video"), m("fal-ai/kling-video/v4/standard/image-to-video", "image-to-video"),
		m("fal-ai/minimax/hailuo-2.3-fast/standard/image-to-video", "image-to-video"),
		m("fal-ai/minimax/hailuo-2.3-fast/pro/image-to-video", "image-to-video"),
		m("bytedance/seedream/v5/lite/text-to-image", "text-to-image"), m("bytedance/seedream/v5.5/text-to-image", "text-to-image"),
		m("fal-ai/nano-banana-pro", "text-to-image"), m("fal-ai/nano-banana-2", "text-to-image"),
		m("creatify/boreal", "image-to-video"),
	}
	cases := []struct {
		current string
		list    []Model
		want    string
	}{
		{"eleven_v4", eleven, "eleven_v5"},             // v6 is an alpha; multilingual is another family
		{"eleven_v4_turbo", eleven, "eleven_v5_turbo"}, // turbo stays turbo
		{"eleven_v5", eleven, ""},                      // already the newest released build
		{"elevenlabs/tts/eleven-v4", fal, ""},          // no v5 of the premium variant yet; v6 is a preview
		{"elevenlabs/tts/eleven-v4-turbo", fal, "elevenlabs/tts/eleven-v5-turbo"},
		// v4 exists as text to video (another tier) and as standard (another variant): v3.5 pro wins.
		{"fal-ai/kling-video/v3/pro/image-to-video", fal, "fal-ai/kling-video/v3.5/pro/image-to-video"},
		{"fal-ai/minimax/hailuo-2.3-fast/standard/image-to-video", fal, ""}, // pro is a variant, not a version
		{"bytedance/seedream/v5/lite/text-to-image", fal, ""},               // v5.5 dropped "lite": another variant
		{"fal-ai/nano-banana-pro", fal, ""},                                 // no version in the id
		{"creatify/boreal", fal, ""},
		{"", fal, ""},
	}
	for _, c := range cases {
		if got := NewerInFamily(c.current, c.list); got != c.want {
			t.Errorf("%q: got %q, want %q", c.current, got, c.want)
		}
	}
}

func TestFamily(t *testing.T) {
	if Family("fal-ai/kling-video/v3.5/pro/image-to-video") != Family("fal-ai/kling-video/v3/pro/image-to-video") {
		t.Error("versions differ only in numbers")
	}
	if Family("eleven_v4") == Family("eleven_v4_turbo") {
		t.Error("turbo is its own family")
	}
}

func TestDescribeFal(t *testing.T) {
	page := []byte(`{"models":[
		{"endpoint_id":"elevenlabs/tts/eleven-v4","metadata":{"category":"text-to-speech","status":"active","date":"2026-09-28T16:35:18.269Z"}},
		{"endpoint_id":"fal-ai/old","metadata":{"category":"text-to-image","status":"deprecated"}}],
		"has_more":true,"next_cursor":"c2"}`)
	models, next := DescribeFal(page)
	if next != "c2" || len(models) != 1 || models[0].Category != "text-to-speech" || models[0].Created == 0 {
		t.Fatalf("got %+v %q", models, next)
	}
}
