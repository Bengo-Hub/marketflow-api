package creative

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// The logo on every creative (owner, 2026-11-24): marketflow-ai draws the tenant's logo on an
// adaptive plate (light for a dark logo, dark for a light one, tinted by the brand's colours while
// the logo stays readable) on posters, motion posters, the end card and, small in a calm top
// corner, on every video scene. These service-config keys (tenant over platform) set how; the
// constants below are only the documented fallbacks.
const (
	LogoPlateStyleKey   = "creative.logo_plate_style"   // auto | light | dark | none
	LogoPlateOpacityKey = "creative.logo_plate_opacity" // 0.5 to 1
	LogoEverySceneKey   = "creative.logo_every_scene"   // true | false
)

// Fallbacks used only when the platform has not set the keys above.
const (
	DefaultLogoPlateStyle   = "auto"
	DefaultLogoPlateOpacity = 0.86
	DefaultLogoEveryScene   = true
)

// logoPlate is marketflow-ai's media.PlateStyle.
type logoPlate struct {
	Style   string   `json:"style,omitempty"`
	Opacity float64  `json:"opacity,omitempty"`
	Tints   []string `json:"tints,omitempty"`
}

var hexColour = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// logoPlateFor reads the plate settings and the brand's colours (the brief's brand snapshot of
// the tenant profile, which auth owns: accent and palette).
func logoPlateFor(ctx context.Context, db *ent.Client, tenantID uuid.UUID, brief *ent.CreativeBrief) logoPlate {
	p := logoPlate{Style: logoPlateStyle(svcconfig.Value(ctx, db, tenantID, LogoPlateStyleKey)),
		Opacity: logoPlateOpacity(svcconfig.Value(ctx, db, tenantID, LogoPlateOpacityKey))}
	if brief != nil {
		brand, _ := brief.Metadata["brand"].(map[string]any)
		p.Tints = brandColours(str(brand["accent"]), strs(brand["palette"]))
	}
	return p
}

func logoPlateStyle(v string) string {
	switch v = strings.ToLower(strings.TrimSpace(v)); v {
	case "auto", "light", "dark", "none":
		return v
	}
	return DefaultLogoPlateStyle
}

func logoPlateOpacity(v string) float64 {
	if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0.5 && f <= 1 {
		return f
	}
	return DefaultLogoPlateOpacity
}

// brandColours are the brand's #rrggbb colours, accent first, without repeats (at most 8).
func brandColours(accent string, palette []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range append([]string{accent}, palette...) {
		c = strings.ToLower(strings.TrimSpace(c))
		if !hexColour.MatchString(c) || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
		if len(out) == 8 {
			break
		}
	}
	return out
}

// logoEveryScene reports whether a video ad lays the logo small over every scene.
func logoEveryScene(ctx context.Context, db *ent.Client, tenantID uuid.UUID) bool {
	if b, err := strconv.ParseBool(strings.TrimSpace(svcconfig.Value(ctx, db, tenantID, LogoEverySceneKey))); err == nil {
		return b
	}
	return DefaultLogoEveryScene
}
