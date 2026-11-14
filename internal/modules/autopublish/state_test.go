package autopublish

import (
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/google/uuid"
)

// The pipeline's one decision: a person's decision ends the agent's part, a flag or a held
// category falls back to approve-each, and otherwise storyboard, its approval, render, schedule.
func TestNextStep(t *testing.T) {
	ok := snapshot{Status: creative.StatusScriptReady, Presenter: true, HasScript: true, AgentOK: true}
	cases := []struct {
		name string
		mod  func(s *snapshot)
		want string
	}{
		{"storyboard first", func(s *snapshot) {}, stepStoryboard},
		{"a template ad renders straight from its script", func(s *snapshot) { s.Presenter = false }, stepRender},
		{"a template ad is still held when flagged", func(s *snapshot) { s.Presenter, s.Flags = false, []string{"x"} }, stepHold},
		{"the agent approves the stills", func(s *snapshot) { s.Storyboard = true }, stepApproveStoryboard},
		{"render once approved", func(s *snapshot) { s.Storyboard, s.StoryboardApproved = true, true }, stepRender},
		{"schedule a finished video", func(s *snapshot) { s.Status = creative.StatusReady }, stepSchedule},
		{"wait while rendering", func(s *snapshot) { s.Status = creative.StatusRendering }, stepWait},
		{"wait while queued", func(s *snapshot) { s.Status = creative.StatusQueued }, stepWait},
		{"failed", func(s *snapshot) { s.Status = creative.StatusFailed }, stepFailed},
		{"a person approved it", func(s *snapshot) { s.Status = creative.StatusApproved }, stepHandOver},
		{"a person discarded it", func(s *snapshot) { s.Status = creative.StatusRejected }, stepHandOver},
		{"restricted or flagged", func(s *snapshot) { s.Flags = []string{"health"} }, stepHold},
		{"flagged after the render too", func(s *snapshot) { s.Status, s.Flags = creative.StatusReady, []string{"x"} }, stepHold},
		{"the tenant's hold category", func(s *snapshot) { s.HeldCategory = "alcohol" }, stepHold},
		{"the agent no longer allows it", func(s *snapshot) { s.AgentOK, s.AgentWhy = false, "switched off" }, stepHold},
		{"no script", func(s *snapshot) { s.HasScript = false }, stepHold},
		{"the storyboard failed", func(s *snapshot) { s.StoryboardError = "no photos" }, stepHold},
	}
	for _, c := range cases {
		s := ok
		c.mod(&s)
		if got, _ := nextStep(s); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	if _, why := nextStep(snapshot{Status: creative.StatusReady, AgentOK: true, HeldCategory: "alcohol"}); !strings.Contains(why, "alcohol") {
		t.Fatalf("the hold reason names the category: %q", why)
	}
}

func TestReadStateAndSteps(t *testing.T) {
	if readState(&ent.CreativeVersion{Metadata: map[string]any{}}) != nil {
		t.Fatal("a version without metadata.autopilot is not the pipeline's")
	}
	at := time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC)
	v := &ent.CreativeVersion{Metadata: map[string]any{creative.MetaAutopilot: map[string]any{
		"agent_id": "a1", "run_id": "r1", "state": StateScheduled, "veto_hours": float64(4),
		"publish_at": at.Format(time.RFC3339), "job_ids": []any{"j1", "j2"},
	}}}
	st := readState(v)
	if st == nil || st.AgentID != "a1" || st.VetoHours != 4 || !st.PublishAt.Equal(at) || len(st.JobIDs) != 2 {
		t.Fatalf("state: %+v", st)
	}
	raw := map[string]any{}
	for i := 0; i < maxSteps+5; i++ {
		raw["steps"] = withStep(raw, map[string]any{"step": i})
	}
	steps := raw["steps"].([]any)
	if len(steps) != maxSteps || steps[len(steps)-1].(map[string]any)["step"] != maxSteps+4 {
		t.Fatalf("the step log keeps the newest %d: %d", maxSteps, len(steps))
	}
}

func TestCaption(t *testing.T) {
	v := &ent.CreativeVersion{Copy: map[string]any{
		"script":   map[string]any{"hooks": []any{"Dry skin? ", "other"}},
		"end_card": map[string]any{"headline": "Shea butter 250g", "cta": "Order on WhatsApp"},
	}}
	if got := Caption(v, nil); got != "Dry skin?\n\nShea butter 250g\n\nOrder on WhatsApp" {
		t.Fatalf("caption: %q", got)
	}
	if got := Caption(&ent.CreativeVersion{}, &ent.CreativeBrief{Title: "Launch"}); got != "Launch" {
		t.Fatalf("fallback caption: %q", got)
	}
}

// Pharmacy hand-off 2026-11-14: an approved medicine ad's caption always ends with its PPB
// approval line, even when the words would fill the length limit.
func TestCaptionCarriesThePPBLine(t *testing.T) {
	ppb := map[string]any{creative.MetaPPBApproval: map[string]any{"reference": "PPB/ADV/123", "caption_line": "PPB approval ref. PPB/ADV/123"}}
	v := &ent.CreativeVersion{Metadata: ppb, Copy: map[string]any{"end_card": map[string]any{"headline": "Panadol 500mg", "cta": "Order on WhatsApp"}}}
	if got := Caption(v, nil); got != "Panadol 500mg\n\nOrder on WhatsApp\n\nPPB approval ref. PPB/ADV/123" {
		t.Fatalf("caption: %q", got)
	}
	long := &ent.CreativeVersion{Metadata: ppb, Copy: map[string]any{"end_card": map[string]any{"headline": strings.Repeat("a", 2500)}}}
	got := Caption(long, nil)
	if len([]rune(got)) > 2000 || !strings.HasSuffix(got, "PPB approval ref. PPB/ADV/123") {
		t.Fatalf("the line survives the limit: %d %q", len([]rune(got)), got[len(got)-40:])
	}
}

// The heads-up says what, where and when, carries the cancel link and never offers a paid boost.
func TestBuildHeadsUp(t *testing.T) {
	at := time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC)
	hu := BuildHeadsUp("Weekly ads", "Shea butter", "Dry skin?", []string{"Facebook (Shea Co)", "Instagram (@shea)"}, at, eastAfrica,
		"https://mf.example/shea/studio/b1", "https://mf.example/veto/tok", true)
	for _, want := range []string{"Weekly ads", "Shea butter", "Facebook (Shea Co), Instagram (@shea)", "Monday 16 November at 14:00",
		"Cancel this post: https://mf.example/veto/tok", "Dry skin?", "never starts a paid boost", "veto window passed"} {
		if !strings.Contains(hu.Body, want) {
			t.Errorf("heads-up body misses %q:\n%s", want, hu.Body)
		}
	}
	if !strings.Contains(hu.Subject, "16 Nov at 14:00") {
		t.Errorf("subject: %q", hu.Subject)
	}
	if strings.Contains(BuildHeadsUp("a", "b", "", nil, at, eastAfrica, "s", "c", false).Body, "boost") {
		t.Error("an organic-only agent's heads-up does not talk about boosts")
	}
}

// The agent-off notice says how many posts were cancelled, why, and what had already gone.
func TestBuildAgentOffNotice(t *testing.T) {
	body := BuildAgentOffNotice("Shea butter", "the agent was switched off", 2, []string{"Facebook (Shea Co)"}, "https://mf.example/s/studio/b1")
	for _, want := range []string{"2 scheduled post(s)", "Shea butter", "the agent was switched off", "Facebook (Shea Co)", "https://mf.example/s/studio/b1"} {
		if !strings.Contains(body, want) {
			t.Errorf("notice misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(BuildAgentOffNotice("a", "b", 1, nil, "c"), "Already on its way") {
		t.Error("nothing had gone, so the notice does not say so")
	}
}

// Cancel links open only with the right key, before their expiry, and never as a review link.
func TestVetoLinks(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	l := NewVetoLinks([]byte("0123456789abcdef0123456789abcdef"))
	l.now = func() time.Time { return now }
	id := uuid.New()
	tok := l.Sign(id, now.Add(time.Hour))
	if got, err := l.Open(tok); err != nil || got != id {
		t.Fatalf("open: %v %v", got, err)
	}
	l.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := l.Open(tok); err != ErrVetoLink {
		t.Fatalf("an expired link must not open: %v", err)
	}
	l.now = func() time.Time { return now }
	other := NewVetoLinks([]byte("another key, another service....."))
	if _, err := other.Open(tok); err != ErrVetoLink {
		t.Fatal("a link signed with another key must not open")
	}
	forged := []byte(tok)
	forged[3] ^= 1
	if _, err := l.Open(string(forged)); err != ErrVetoLink {
		t.Fatal("a changed link must not open")
	}
	if NewVetoLinks(nil) != nil {
		t.Fatal("no key, no links")
	}
	var none *VetoLinks
	if _, err := none.Open(tok); err != ErrVetoLink {
		t.Fatal("a missing signer opens nothing")
	}
}
