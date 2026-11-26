package creative

import "github.com/bengobox/marketflow-api/internal/ent"

// CleanScenes are a version's drawn pictures without words, a placed logo or a screenshot: a poster's
// scene before its text, and the stills of the storyboard in use (the twin without the logo when one
// was placed). A still with writing in it, one that shows a screen, the presenter's own picture and
// the end card preview are left out.
func CleanScenes(v *ent.CreativeVersion, assets []*ent.CreativeAsset) []*ent.CreativeAsset {
	var out []*ent.CreativeAsset
	if v.Kind == KindPoster {
		for _, a := range assets {
			if a.Kind == AssetScene {
				out = append(out, a)
			}
		}
		return out
	}
	byKey := map[string]*ent.CreativeAsset{}
	for _, a := range assets {
		byKey[a.StorageKey] = a
	}
	sb, _ := v.Metadata["storyboard"].(map[string]any)
	stills, _ := sb["stills"].([]any)
	for _, x := range stills {
		st, _ := x.(map[string]any)
		screen, _ := st["screen"].(map[string]any)
		if str(st["source"]) != "generated" || str(st["cue"]) != "" || len(screen) > 0 {
			continue
		}
		key := str(st["key"])
		if logo, _ := st["logo"].(map[string]any); len(logo) > 0 || str(st["plain_key"]) != "" {
			key = str(st["plain_key"])
		}
		if a := byKey[key]; a != nil && a.Kind == AssetKeyframe {
			out = append(out, a)
		}
	}
	return out
}
