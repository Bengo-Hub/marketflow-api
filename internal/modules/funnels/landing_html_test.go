package funnels

import (
	"strings"
	"testing"
)

func TestSanitizeLandingHTML(t *testing.T) {
	in := `<section class="hero" style="color:#fff;position:fixed"><h1 onclick="x()">Hi</h1><script>alert(1)</script><img src="https://cdn.example.com/a.jpg" onerror="x()"><a href="javascript:alert(1)">bad</a><iframe src="https://evil"></iframe></section>`
	out := SanitizeLandingHTML(in)
	for _, banned := range []string{"<script", "onclick", "onerror", "javascript:", "<iframe", "position"} {
		if strings.Contains(out, banned) {
			t.Errorf("%q survived: %s", banned, out)
		}
	}
	for _, kept := range []string{`<section class="hero"`, "<h1>Hi</h1>", `src="https://cdn.example.com/a.jpg"`, "color: #fff"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q was dropped: %s", kept, out)
		}
	}
}

func TestSafeColor(t *testing.T) {
	for in, want := range map[string]string{
		"#9100B0":                  "#9100B0",
		" #abc ":                   "#abc",
		"red":                      "#000",
		"#12345":                   "#000",
		"#fff;}</style><script>x(": "#000",
	} {
		if got := SafeColor(in, "#000"); got != want {
			t.Errorf("SafeColor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLegacyFieldsFold(t *testing.T) {
	head, color := "Join the challenge", "#16a34a"
	l := legacyFields{Headline: &head}
	l.ContentJSON = &struct {
		Steps []map[string]any `json:"steps"`
		Theme map[string]any   `json:"theme"`
	}{Steps: []map[string]any{{"block_type": "form"}}, Theme: map[string]any{"brand_color": color}}
	steps, meta := l.fold(nil, nil)
	if len(steps) != 1 || meta[MetaHeadline] != head || meta[MetaBrandColor] != color {
		t.Fatalf("fold = %v %v", steps, meta)
	}
	// Canonical values win.
	steps, meta = l.fold([]map[string]any{}, map[string]any{MetaHeadline: "Mine"})
	if len(steps) != 0 || meta[MetaHeadline] != "Mine" {
		t.Fatalf("canonical overwritten: %v %v", steps, meta)
	}
}
