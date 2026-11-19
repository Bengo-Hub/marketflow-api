package modelcatalog

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Role is one job a provider's model does, keyed by the provider setting it fills (main_model,
// router_model, voice_model, ...). Families are regular expressions, matched case-insensitively,
// in order of preference: the newest stable model of the first family with a match is
// recommended. Exclude drops more ids than the stable filter does.
type Role struct {
	Label    string   `json:"label"`
	Families []string `json:"families"`
	Exclude  []string `json:"exclude,omitempty"`
	// KeepCurrent keeps the configured model when the provider still lists it (Ollama: an
	// installed model is a deliberate choice, and a newer tag may be a smaller model).
	KeepCurrent bool `json:"keep_current,omitempty"`
	// Informational roles are shown as the newest model per family but are not a setting:
	// the media routes that use them are fixed in marketflow-ai.
	Informational bool `json:"informational,omitempty"`
}

// DefaultRoles is the documented fallback list. Roles cover every tier a provider fills:
// cheap or free first (router, fallback, standard voice), then standard and premium.
var DefaultRoles = map[string]map[string]Role{
	"groq": {
		"router_model":   {Label: "Router (cheap)", Families: []string{`gpt-oss-20b`, `llama-[\d.]+-8b`, `\b8b\b`}},
		"main_model":     {Label: "Responder (standard)", Families: []string{`gpt-oss-120b`, `llama-.*70b`, `\b70b\b`}},
		"fallback_model": {Label: "Fallback (cheap)", Families: []string{`gpt-oss-20b`, `llama-[\d.]+-8b`, `\b8b\b`}},
	},
	"openai": {
		"main_model":    {Label: "Standard", Families: []string{`^gpt-\d+(\.\d+)?-mini$`, `^gpt-\d+o-mini$`}},
		"premium_model": {Label: "Premium", Families: []string{`^gpt-\d+(\.\d+)?$`, `^gpt-\d+o$`}},
	},
	"anthropic": {
		"main_model": {Label: "Standard and premium (Haiku)", Families: []string{`haiku`}},
		"plus_model": {Label: "Premium plus (Sonnet)", Families: []string{`sonnet`}},
	},
	"ollama": {
		"fallback_model":  {Label: "Local chat (free)", Families: []string{`^llama`, `^qwen`, `^gemma`}, KeepCurrent: true},
		"embedding_model": {Label: "Embeddings", Families: []string{`embed`}, KeepCurrent: true},
	},
	"elevenlabs": {
		"voice_model":         {Label: "Voice (standard)", Families: []string{`^eleven_v\d+_turbo$`, `^eleven_turbo_v[\d_]+$`, `^eleven_flash_v[\d_]+$`}},
		"voice_premium_model": {Label: "Voice (premium)", Families: []string{`^eleven_v\d+$`, `^eleven_multilingual_v[\d_]+$`}},
	},
	"fal": {
		"image_standard": {Label: "Poster image (standard)", Families: []string{`seedream`}, Informational: true},
		"image_premium":  {Label: "Poster image (premium)", Families: []string{`nano-banana-pro`, `nano-banana`}, Informational: true},
		"video_standard": {Label: "Video clip (standard)", Families: []string{`hailuo.*fast`, `hailuo`}, Informational: true},
		"video_premium":  {Label: "Video clip (premium)", Families: []string{`seedance`, `kling.*pro`}, Informational: true},
		"presenter":      {Label: "Presenter", Families: []string{`creatify/boreal`, `veed/fabric`}, Informational: true},
	},
}

// unstable marks ids that are not a stable, general model: previews, betas, dated experiments and
// single-purpose variants (audio, speech, search, moderation).
var unstable = regexp.MustCompile(`(?i)(preview|beta|alpha|\bexp\b|experimental|latest|\brc\d*\b|realtime|audio|transcribe|tts|whisper|search|moderation|guard|instruct|codex|deep-research|computer-use|-chat$|chat-)`)

// RolesFor returns a provider's roles with the platform's overrides (JSON as in the package
// doc) applied role by role.
func RolesFor(provider, overrides string) map[string]Role {
	out := map[string]Role{}
	for k, r := range DefaultRoles[provider] {
		out[k] = r
	}
	if strings.TrimSpace(overrides) == "" {
		return out
	}
	var over map[string]map[string]Role
	if json.Unmarshal([]byte(overrides), &over) != nil {
		return out
	}
	for k, r := range over[provider] {
		if len(r.Families) == 0 {
			delete(out, k)
			continue
		}
		out[k] = r
	}
	return out
}

// Recommendation is the model picked for one role.
type Recommendation struct {
	Field         string `json:"field"`
	Label         string `json:"label"`
	Model         string `json:"model"`
	Informational bool   `json:"informational,omitempty"`
}

// Recommend picks a model from ids for every role it can fill. current is the provider's saved
// settings, read for KeepCurrent roles.
func Recommend(ids []string, roles map[string]Role, current map[string]string) []Recommendation {
	live := map[string]bool{}
	for _, id := range ids {
		live[id] = true
	}
	var out []Recommendation
	for field, role := range roles {
		if role.KeepCurrent && live[current[field]] {
			out = append(out, Recommendation{Field: field, Label: role.Label, Model: current[field], Informational: role.Informational})
			continue
		}
		if m := pick(ids, role); m != "" {
			out = append(out, Recommendation{Field: field, Label: role.Label, Model: m, Informational: role.Informational})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

func pick(ids []string, role Role) string {
	var excl []*regexp.Regexp
	for _, e := range role.Exclude {
		if re, err := regexp.Compile("(?i)" + e); err == nil {
			excl = append(excl, re)
		}
	}
	for _, fam := range role.Families {
		re, err := regexp.Compile("(?i)" + fam)
		if err != nil {
			continue
		}
		var cands []string
	next:
		for _, id := range ids {
			if !re.MatchString(id) || unstable.MatchString(id) {
				continue
			}
			for _, x := range excl {
				if x.MatchString(id) {
					continue next
				}
			}
			cands = append(cands, id)
		}
		if len(cands) == 0 {
			continue
		}
		sort.Slice(cands, func(i, j int) bool { return newer(cands[i], cands[j]) })
		return cands[0]
	}
	return ""
}

var (
	dateRe = regexp.MustCompile(`(19|20)\d{2}-?\d{2}-?\d{2}`)
	sizeRe = regexp.MustCompile(`(?i)\b\d+(\.\d+)?[bm]\b`)
	numRe  = regexp.MustCompile(`\d+`)
)

// version reads an id's version numbers (sizes such as 120b and snapshot dates left out) and
// its snapshot date.
func version(id string) (nums []int, date string) {
	if d := dateRe.FindString(id); d != "" {
		date = strings.ReplaceAll(d, "-", "")
		id = strings.Replace(id, d, "", 1)
	}
	id = sizeRe.ReplaceAllString(id, "")
	for _, s := range numRe.FindAllString(id, -1) {
		n, _ := strconv.Atoi(s)
		nums = append(nums, n)
	}
	return nums, date
}

// newer orders a before b when a is the newer release: higher version numbers first; on a tie an
// undated alias before a dated snapshot, then the newest snapshot, then the shorter id.
func newer(a, b string) bool {
	va, da := version(a)
	vb, db := version(b)
	for i := 0; i < len(va) || i < len(vb); i++ {
		x, y := -1, -1
		if i < len(va) {
			x = va[i]
		}
		if i < len(vb) {
			y = vb[i]
		}
		if x != y {
			return x > y
		}
	}
	if (da == "") != (db == "") {
		return da == ""
	}
	if da != db {
		return da > db
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
