package creative

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// ScreensKey is the real product screen rules (owner, 2026-11-24, plan item 66 e): a scene that
// shows a laptop, tablet, POS or phone carries one of the tenant's own screenshots (brand images
// marked as screenshots), composited by marketflow-ai and checked by its self-review
// (media/screens.go). JSON, tenant over platform. Nothing happens for a tenant with no screenshots.
const ScreensKey = "creative.screens"

// DefaultScreens is the fallback while the platform sets none: at most two scenes.
const DefaultScreens = `{"enabled":true,"max_scenes":2}`

// screenRules are the rules sent with a storyboard.
type screenRules struct {
	Enabled   bool `json:"enabled"`
	MaxScenes int  `json:"max_scenes,omitempty"`
}

// screenshot is one of the tenant's screenshots with a short-lived link.
type screenshot struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Device string `json:"device,omitempty"`
}

// screensFor is the rules and the tenant's own screenshots; none when off or when it has none.
func (r *Renderer) screensFor(ctx context.Context, tenantID uuid.UUID) (*screenRules, []screenshot) {
	if r.brandLinks == nil {
		return nil, nil
	}
	var rules screenRules
	if json.Unmarshal([]byte(svcconfig.Value(ctx, r.db, tenantID, ScreensKey)), &rules) != nil {
		_ = json.Unmarshal([]byte(DefaultScreens), &rules)
	}
	if !rules.Enabled {
		return nil, nil
	}
	t, err := r.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return nil, nil
	}
	var shots []screenshot
	for _, img := range brandimages.OfKind(t.Metadata, tenantID, brandimages.KindScreenshot) {
		if links := r.brandLinks.Links(tenantID, []string{img.Key}); len(links) == 1 {
			shots = append(shots, screenshot{ID: img.ID, URL: links[0], Device: img.Device})
		}
	}
	if len(shots) == 0 {
		return nil, nil
	}
	return &rules, shots
}

// staticScenes are the scenes of a version's storyboard whose still carries a screenshot: they are
// rendered as stills (marketflow-ai media/screens.go).
func staticScenes(v *ent.CreativeVersion) []string {
	sb, _ := v.Metadata["storyboard"].(map[string]any)
	stills, _ := sb["stills"].([]any)
	var out []string
	for _, x := range stills {
		st, _ := x.(map[string]any)
		if sc, _ := st["screen"].(map[string]any); str(sc["id"]) != "" {
			out = append(out, str(st["scene_id"]))
		}
	}
	return out
}
