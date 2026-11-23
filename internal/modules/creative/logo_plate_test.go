package creative

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLogoPlateSettingsFallBack(t *testing.T) {
	for in, want := range map[string]string{"": "auto", "Light": "light", " dark ": "dark", "none": "none", "glow": "auto"} {
		if got := logoPlateStyle(in); got != want {
			t.Fatalf("style %q gave %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]float64{"": 0.86, "0.8": 0.8, "1": 1, "0.2": 0.86, "abc": 0.86, "1.5": 0.86} {
		if got := logoPlateOpacity(in); got != want {
			t.Fatalf("opacity %q gave %v, want %v", in, got, want)
		}
	}
}

// The plate is tinted by the brand's own colours only: valid hex, accent first, no repeats.
func TestBrandColours(t *testing.T) {
	got := brandColours("#6A2456", []string{"#f3e8ff", "#6a2456", "purple", "", "#12345"})
	if want := []string{"#6a2456", "#f3e8ff"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("colours %v, want %v", got, want)
	}
	if brandColours("", nil) != nil {
		t.Fatal("no brand colours should leave the plate neutral")
	}
}

// The end card words travel flat in the storyboard and the recompose bodies, with the plate.
func TestEndCardWordsAreFlatInTheCalls(t *testing.T) {
	w := endCardWords{Headline: "Codevertex POS", CTA: "Talk to us", Contacts: []contact{{Kind: "email", Value: "info@example.com"}},
		LogoPlate: logoPlate{Style: "auto", Opacity: 0.86, Tints: []string{"#6a2456"}}}
	for _, body := range []any{storyboardCall{endCardWords: w}, endCardPreviewCall{SceneID: "s6", StillURL: "https://x/s6.png", endCardWords: w}} {
		b, _ := json.Marshal(body)
		s := string(b)
		for _, k := range []string{`"headline":"Codevertex POS"`, `"kind":"email"`, `"logo_plate":{"style":"auto","opacity":0.86,"tints":["#6a2456"]}`} {
			if !strings.Contains(s, k) {
				t.Fatalf("%s missing in %s", k, s)
			}
		}
	}
	b, _ := json.Marshal(adCall{LogoEveryScene: true})
	if !strings.Contains(string(b), `"logo_every_scene":true`) {
		t.Fatalf("ad call %s", b)
	}
}
