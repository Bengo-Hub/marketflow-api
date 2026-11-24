package creative

import (
	"reflect"
	"testing"
)

func TestApplySceneSettingsChangesOnlyMovedScenes(t *testing.T) {
	scenes := []ScriptScene{
		{ID: "s1", Shot: "b_roll", Setting: "a bright learning room, Kisumu CBD", Voiceover: "Hi"},
		{ID: "s2", Shot: "b_roll", Setting: "a mall office reception"},
		{ID: "s3", Shot: "owner_footage"},
		{ID: "s4", Shot: "end_card", Setting: "a family home"},
	}
	changed, keep := applySceneSettings(scenes, map[string]string{
		"s1": "a bright learning room, Kisumu CBD", "s2": "Oginga-Odinga Road with matatus passing", "s3": "anywhere"})
	if changed != 1 || !reflect.DeepEqual(keep, []string{"s1", "s3", "s4"}) {
		t.Fatalf("changed %d keep %v", changed, keep)
	}
	if scenes[1].Setting != "Oginga-Odinga Road with matatus passing" || scenes[0].Voiceover != "Hi" || scenes[2].Setting != "" {
		t.Fatalf("scenes %+v", scenes)
	}
}

// keep_unchanged sends the stills it can: stored prompts always, prompt-less ones only for the
// scenes a re-set left as they were.
func TestPreviousStills(t *testing.T) {
	meta := map[string]any{
		"storyboard": map[string]any{"stills": []any{
			map[string]any{"scene_id": "s1", "source": "generated", "url": "https://x/1.jpg", "key": "k1"},
			map[string]any{"scene_id": "s2", "source": "generated", "url": "https://x/2.jpg", "key": "k2"},
			map[string]any{"scene_id": "s3", "source": "generated", "url": "https://x/3.jpg", "prompt": "p3"},
			map[string]any{"scene_id": "s4", "source": "photo", "url": "https://x/4.jpg"},
		}},
		metaKeepScenes: []any{"s1"},
	}
	if previousStills(meta) != nil {
		t.Fatal("without keep_unchanged nothing is reused")
	}
	meta[metaKeepUnchanged] = true
	got := previousStills(meta)
	if len(got) != 2 || got[0].SceneID != "s1" || got[0].Prompt != "" || got[1].SceneID != "s3" || got[1].Prompt != "p3" {
		t.Fatalf("previous %+v", got)
	}
}
