package creative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
)

// Settings re-set (owner, 2026-11-24): when the place facts or the landmark registry improve, a
// script waiting for review is placed again without being rewritten. marketflow-ai writes a fresh
// setting brief and gives each scene its setting; only the settings change. The scenes whose
// setting is unchanged keep their stills (metaKeepScenes), so the next storyboard, made with
// keep_unchanged, draws only the scenes that moved. The plan then waits for the owner again.

const (
	// metaKeepUnchanged asks the next storyboard to reuse the stills of unchanged scenes.
	metaKeepUnchanged = "keep_unchanged"
	// metaKeepScenes are the scene ids a settings re-set left as they were.
	metaKeepScenes = "keep_scenes"
)

// settingsCall is marketflow-ai's POST /api/v1/internal/creative/script/settings body.
type settingsCall struct {
	TenantID  string        `json:"tenant_id"`
	VersionID string        `json:"version_id"`
	Offering  string        `json:"offering"`
	Facts     string        `json:"facts"`
	Objective string        `json:"objective"`
	Audience  string        `json:"audience"`
	Language  string        `json:"language"`
	Format    string        `json:"format,omitempty"`
	Place     *placeFacts   `json:"place,omitempty"`
	Scenes    []ScriptScene `json:"scenes"`
}

// ResetSettings places the script's scenes again and returns the version and how many scenes
// changed setting. Nothing is charged and nothing is drawn here.
func (r *Renderer) ResetSettings(ctx context.Context, v *ent.CreativeVersion) (*ent.CreativeVersion, int, error) {
	var script VideoScript
	if v.Kind != KindVideoAd || v.Status != StatusScriptReady {
		return nil, 0, fmt.Errorf("%w: only a video ad script waiting for review can be placed again", ErrConflict)
	}
	if b, err := json.Marshal(v.Copy["script"]); err != nil || json.Unmarshal(b, &script) != nil || len(script.Scenes) == 0 {
		return nil, 0, fmt.Errorf("%w: this video ad has no script", ErrConflict)
	}
	b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx)
	if err != nil {
		return nil, 0, err
	}
	name := strings.TrimSpace(str(b.Offering["name"]))
	if name == "" {
		name = b.Title
	}
	call := settingsCall{TenantID: v.TenantID.String(), VersionID: v.ID.String(), Offering: excerpt(name, 300),
		Facts: offeringFacts(b.Offering), Objective: b.Objective, Audience: b.Audience, Language: str(v.Metadata["language"]),
		Format: v.Format, Scenes: script.Scenes}
	if p := r.resolvePlace(ctx, v.TenantID, b.Offering, true); p.Facts.Address != "" || len(p.Facts.Pages) > 0 || len(p.Facts.Pictures) > 0 {
		call.Place = &p.Facts
	}
	data, status, err := r.aiCall(ctx, http.MethodPost, "/api/v1/internal/creative/script/settings", call, 90*time.Second)
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusOK {
		return nil, 0, aiStatusError(status, data)
	}
	var reply struct {
		Scenes []struct {
			ID      string `json:"id"`
			Setting string `json:"setting"`
		} `json:"scenes"`
		SettingBrief map[string]any `json:"setting_brief"`
	}
	if json.Unmarshal(data, &reply) != nil {
		return nil, 0, errors.New("marketflow-ai sent unreadable settings")
	}
	settings := map[string]string{}
	for _, s := range reply.Scenes {
		settings[s.ID] = s.Setting
	}
	changed, keep := applySceneSettings(script.Scenes, settings)
	if changed == 0 {
		return v, 0, nil
	}
	copyFields := copyMap(v.Copy)
	copyFields["script"] = toMap(script)
	meta := copyMap(v.Metadata)
	if len(reply.SettingBrief) > 0 {
		meta[metaSettingBrief] = reply.SettingBrief
	}
	meta[metaKeepScenes] = keep
	n, err := r.db.CreativeVersion.Update().
		Where(creativeversion.ID(v.ID), creativeversion.StatusEQ(StatusScriptReady), creativeversion.UpdatedAt(v.UpdatedAt)).
		SetCopy(copyFields).SetMetadata(meta).Save(ctx)
	if err != nil {
		return nil, 0, err
	}
	if n == 0 {
		return nil, 0, fmt.Errorf("%w: the version changed meanwhile; try again", ErrConflict)
	}
	out, err := r.db.CreativeVersion.Get(ctx, v.ID)
	return out, changed, err
}

// applySceneSettings sets each scene's new setting (owner footage keeps none) and returns how many
// changed and the ids of those that did not.
func applySceneSettings(scenes []ScriptScene, settings map[string]string) (changed int, keep []string) {
	for i := range scenes {
		s := &scenes[i]
		next, ok := settings[s.ID]
		if !ok || s.Shot == "owner_footage" || strings.TrimSpace(next) == strings.TrimSpace(s.Setting) {
			keep = append(keep, s.ID)
			continue
		}
		s.Setting = strings.TrimSpace(next)
		changed++
	}
	return changed, keep
}

// previousStills are the stills a keep_unchanged storyboard may reuse: those whose stored prompt
// marketflow-ai can compare, and, from a storyboard made before prompts were kept, those of the
// scenes a settings re-set left unchanged (sent without a prompt: this service vouches for them).
func previousStills(meta map[string]any) []storyboardStill {
	if keep, _ := meta[metaKeepUnchanged].(bool); !keep {
		return nil
	}
	sb, _ := meta["storyboard"].(map[string]any)
	list, _ := sb["stills"].([]any)
	kept := map[string]bool{}
	for _, id := range strs(meta[metaKeepScenes]) {
		kept[id] = true
	}
	var out []storyboardStill
	for _, x := range list {
		st, ok := x.(map[string]any)
		if !ok || str(st["source"]) != "generated" || str(st["url"]) == "" {
			continue
		}
		p := storyboardStill{SceneID: str(st["scene_id"]), URL: str(st["url"]), Key: str(st["key"]), Source: "generated",
			Route: str(st["route"]), Prompt: str(st["prompt"])}
		if p.Prompt == "" && !kept[p.SceneID] {
			continue
		}
		out = append(out, p)
	}
	return out
}
