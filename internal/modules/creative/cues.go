package creative

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// LocalCuesKey is the local cue rules (owner, 2026-11-24, plan item 66 d): short, true writing a
// scene may carry (a plain LIPA HAPA card on a counter), drawn by marketflow-ai only on routes the
// platform lists as rendering text well and kept only when its self-review reads it
// (media/cues.go). marks are third parties' trade marks and the neutral words a drawn scene says
// instead: a model is never asked to draw someone else's name or logo. JSON, tenant over platform.
const LocalCuesKey = "creative.local_cues"

// DefaultLocalCues is the fallback while the platform sets none. The words are true in any such
// scene and carry no number, name, price or opening time.
const DefaultLocalCues = `{"enabled":true,"max_scenes":1,"cues":[
{"match":["counter","cashier","checkout"],"object":"a small plain card standing on the counter","text":"LIPA HAPA","countries":["KE","TZ"]},
{"match":["shop door","shop entrance","shopfront","storefront"],"object":"a small plain sign hanging on the shop door","text":"KARIBU","countries":["KE","TZ"]}],
"marks":{"lipa na m-pesa":"mobile money","m-pesa":"mobile money","mpesa":"mobile money","airtel money":"mobile money","t-kash":"mobile money","safaricom":"mobile phone","airtel":"mobile phone","visa card":"bank card","mastercard":"bank card","kra etims":"tax","etims":"tax"}}`

// localCue is one cue; Countries limits it to tenants there (empty: anywhere).
type localCue struct {
	Match     []string `json:"match"`
	Object    string   `json:"object"`
	Text      string   `json:"text"`
	Countries []string `json:"countries,omitempty"`
}

// localCues are the rules sent with a storyboard and a render.
type localCues struct {
	Enabled   bool              `json:"enabled"`
	MaxScenes int               `json:"max_scenes,omitempty"`
	Cues      []localCue        `json:"cues,omitempty"`
	Marks     map[string]string `json:"marks,omitempty"`
}

// localCuesFor is the rules in force with the cues of the tenant's country. A setting that leaves
// out cues or marks keeps the default ones, so turning the cues off never lets a trade mark through.
func localCuesFor(ctx context.Context, db *ent.Client, tenantID uuid.UUID) *localCues {
	var base, c localCues
	_ = json.Unmarshal([]byte(DefaultLocalCues), &base)
	raw := svcconfig.Value(ctx, db, tenantID, LocalCuesKey)
	if raw == "" || json.Unmarshal([]byte(raw), &c) != nil {
		c = base
	}
	if c.Cues == nil {
		c.Cues = base.Cues
	}
	if c.Marks == nil {
		c.Marks = base.Marks
	}
	country := ""
	if t, err := db.Tenant.Get(ctx, tenantID); err == nil && t.Country != nil {
		country = strings.TrimSpace(*t.Country)
	}
	c.Cues = slices.DeleteFunc(slices.Clone(c.Cues), func(x localCue) bool {
		return len(x.Countries) > 0 && !slices.ContainsFunc(x.Countries, func(want string) bool { return strings.EqualFold(want, country) })
	})
	return &c
}

// cueWords is the short writing each still of a version's storyboard carries, by scene.
func cueWords(v *ent.CreativeVersion) map[string]string {
	sb, _ := v.Metadata["storyboard"].(map[string]any)
	stills, _ := sb["stills"].([]any)
	var out map[string]string
	for _, x := range stills {
		if st, _ := x.(map[string]any); str(st["cue"]) != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[str(st["scene_id"])] = str(st["cue"])
		}
	}
	return out
}
