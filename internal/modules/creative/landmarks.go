package creative

import (
	"encoding/json"
	"strings"
)

// Location-true scenes (owner, 2026-11-24): an ad for a business in Kisumu CBD should look like
// Kisumu CBD. The landmark registry is service config creative.landmarks (tenant over platform): a
// JSON object keyed by town or area, each with the landmarks seen from its streets, the look of
// the area and its street life, written so an image model can draw them believably. The script's
// setting brief uses them for exterior and establishing shots and background cues only; the
// business is never placed inside a landmark it is not in. DefaultLandmarks is only the fallback
// seed, and it holds only landmarks checked on the web (2026-11-24).
const (
	LandmarksKey      = "creative.landmarks"
	LandmarkLookupKey = "creative.landmark_lookup" // true | false: marketflow-ai's cached web lookup
)

// DefaultLandmarks is the seed used only when the platform has not set creative.landmarks.
const DefaultLandmarks = `{
  "kisumu cbd": {
    "aliases": ["kisumu"],
    "landmarks": [
      "the twin towers of Mega Plaza on Oginga Odinga Street, the tallest buildings in Kisumu",
      "the old clock tower at the junction of Oginga Odinga Street and Kampala Road",
      "the University of Nairobi Kisumu Campus buildings in the city centre",
      "the Lake Victoria shoreline at the western edge of town"
    ],
    "look": "a compact lakeside town centre of low and mid-rise shops and offices with a few new glass towers, warm lake light",
    "street_life": "matatus and boda bodas along Oginga Odinga Street, tuk-tuks, roadside kiosks and hawkers, busy pavements outside the malls"
  },
  "nairobi cbd": {
    "aliases": ["nairobi central", "nairobi city centre"],
    "landmarks": [
      "the round Kenyatta International Convention Centre (KICC) tower on Harambee Avenue",
      "the Central Bank of Kenya's Times Tower on Haile Selassie Avenue",
      "Afya Centre's light green high-rise on Tom Mboya Street"
    ],
    "look": "a dense high-rise city centre, busy and modern",
    "street_life": "matatus in bright livery, crowded pavements, hawkers, boda bodas at the edges of town"
  }
}`

// areaLandmarks is one registry entry.
type areaLandmarks struct {
	Aliases    []string `json:"aliases,omitempty"`
	Landmarks  []string `json:"landmarks"`
	Look       string   `json:"look,omitempty"`
	StreetLife string   `json:"street_life,omitempty"`
}

// placeArea is marketflow-ai's team.PlaceArea: the registry entry the business's address is in.
type placeArea struct {
	Name       string   `json:"name"`
	Landmarks  []string `json:"landmarks,omitempty"`
	Look       string   `json:"look,omitempty"`
	StreetLife string   `json:"street_life,omitempty"`
}

// normPlace lowercases and treats hyphens and runs of spaces alike, so "Oginga-Odinga Road" and
// "Oginga Odinga Road" are the same place.
func normPlace(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "-", " "))
	return " " + strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r == ',' || r == '.' || r == '(' || r == ')' {
			return ' '
		}
		return r
	}, s)), " ") + " "
}

// areaFor finds the registry entry for an address and town: the longest key or alias found in
// them as whole words, so "kisumu cbd" wins over "kisumu". An unreadable registry falls back to
// the seed; no match gives nil.
func areaFor(registry, address, city string) *placeArea {
	var reg map[string]areaLandmarks
	if json.Unmarshal([]byte(registry), &reg) != nil || len(reg) == 0 {
		if json.Unmarshal([]byte(DefaultLandmarks), &reg) != nil {
			return nil
		}
	}
	where := normPlace(address + " " + city)
	best, bestLen := "", 0
	for key, a := range reg {
		for _, name := range append([]string{key}, a.Aliases...) {
			n := strings.TrimSpace(normPlace(name))
			if n != "" && len(n) > bestLen && strings.Contains(where, " "+n+" ") {
				best, bestLen = key, len(n)
			}
		}
	}
	if best == "" {
		return nil
	}
	a := reg[best]
	out := &placeArea{Name: best, Look: excerpt(a.Look, 200), StreetLife: excerpt(a.StreetLife, 200)}
	for _, l := range a.Landmarks {
		if l = strings.TrimSpace(l); l != "" && len(out.Landmarks) < 6 {
			out.Landmarks = append(out.Landmarks, excerpt(l, 160))
		}
	}
	return out
}
