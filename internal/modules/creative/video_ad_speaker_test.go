package creative

import (
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// The saved actor's gender is read from its record, else its description (owner, 2026-11-14:
// the script follows the actor).
func TestBriefActorGender(t *testing.T) {
	cases := []struct {
		meta map[string]any
		want string
	}{
		{map[string]any{}, ""},
		{map[string]any{"actor": map[string]any{"url": "https://x/a.jpg", "gender": "male", "prompt": "a woman"}}, "male"},
		{map[string]any{"actor": map[string]any{"url": "https://x/a.jpg", "prompt": "a shop owner in her forties"}}, "female"},
		{map[string]any{"actor": map[string]any{"url": "https://x/a.jpg", "prompt": "a friendly presenter"}}, ""},
		{map[string]any{"actor": map[string]any{"prompt": "a man"}}, ""}, // no picture yet: no actor
	}
	for i, c := range cases {
		if got := briefActorGender(c.meta); got != c.want {
			t.Errorf("case %d: got %q, want %q", i, got, c.want)
		}
	}
}

// A two-voice UGC script carries a speaker per scene, an approach and the writer's actor line;
// the studio stores them as given and refuses anything else, so the render gets clean values.
func TestVideoScriptSpeakersAndApproach(t *testing.T) {
	script := func() *VideoScript {
		return &VideoScript{Approach: " Myth-Fact ", Actor: "a woman in her thirties at a duka counter", Scenes: []ScriptScene{
			{ID: "s1", Shot: "product_clip", Voiceover: "Is a till app too dear?", Speaker: "Female"},
			{ID: "s2", Shot: "product_clip", Voiceover: "Not this one.", Speaker: "male"},
			{ID: "s3", Shot: "end_card", Voiceover: "WhatsApp DEMO.", Speaker: ""},
		}}
	}
	v := script()
	if err := v.validate(1, 0, VideoTemplate); err != nil {
		t.Fatalf("a two-voice script was refused: %v", err)
	}
	if v.Scenes[0].Speaker != "female" || v.Approach != "myth-fact" {
		t.Fatalf("not normalised: %+v", v)
	}
	bad := script()
	bad.Scenes[1].Speaker = "robot"
	if err := bad.validate(1, 0, VideoTemplate); err == nil || !strings.Contains(err.Error(), "speaker") {
		t.Fatalf("an unknown speaker must be refused: %v", err)
	}
	bad = script()
	bad.Approach = "myth fact; drop"
	if err := bad.validate(1, 0, VideoTemplate); err == nil {
		t.Fatal("an approach that is not a template key must be refused")
	}
	bad = script()
	bad.Actor = strings.Repeat("a", maxScriptActor+1)
	if err := bad.validate(1, 0, VideoTemplate); err == nil {
		t.Fatal("an over-long actor line must be refused")
	}
}

func TestVideoAdInputApproach(t *testing.T) {
	brief := &ent.CreativeBrief{Language: "en", Offering: map[string]any{"photo_urls": []any{"https://x/p.jpg"}}}
	in := VideoAdInput{Delivery: "ugc", Approach: "Day-In-The-Life"}
	if err := in.validate(brief); err != nil || in.Approach != "day-in-the-life" {
		t.Fatalf("approach = %q, %v", in.Approach, err)
	}
	in = VideoAdInput{Approach: "<script>"}
	if err := in.validate(brief); err == nil {
		t.Fatal("a bad approach must be refused")
	}
	for _, ok := range []string{"", "educate", "problem-agitate-solve"} {
		if !validApproach(ok) {
			t.Errorf("%q must be a valid approach", ok)
		}
	}
}
