package adsetup

import (
	"context"
	"image"
	"regexp"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/platform/adimage"
)

// Pictures for a Performance Max group (plan item 68, owner 2026-11-25): the business's own photos
// and the clean scenes of its approved ads, cut to Google's shapes, shown first and sent only when
// the owner ticks them. Never a logo, a screenshot, a poster or a person's consent picture.

const (
	// pictureKeyPrefix starts the key of a picture field: image|<field type>|<picture>.
	pictureKeyPrefix = "image|"
	// pictureRows is how many pictures one shape offers, and maxPictures how many one fix sends.
	pictureRows = 8
	maxPictures = 10
)

// PictureChoice is one picture the owner may tick; the portal shows it cut as Google will get it.
type PictureChoice struct {
	URL    string  `json:"url"`
	Source string  `json:"source"`
	Ratio  float64 `json:"ratio"` // width over height
}

// Picture is one of the business's pictures. Width and Height are 0 when unknown.
type Picture struct {
	ID, URL, Source string
	Width, Height   int
	load            func(ctx context.Context) (image.Image, error)
}

// posterHint matches pictures with words drawn on them.
var posterHint = regexp.MustCompile(`(?i)(poster|flyer|flier|banner|leaflet|brochure|screenshot|screen shot|end ?card)`)

func looksLikePoster(img brandimages.Image) bool {
	return posterHint.MatchString(img.Name) || posterHint.MatchString(img.Source) || posterHint.MatchString(img.Caption) ||
		posterHint.MatchString(strings.Join(img.Tags, " "))
}

// pictures are the tenant's usable pictures, its own photos first.
func (h *Handler) pictures(ctx context.Context, t *ent.Tenant) []Picture {
	if h.Links == nil || h.Fetch == nil {
		return nil
	}
	var out []Picture
	add := func(id, u, source string, w, hgt int) {
		out = append(out, Picture{ID: id, URL: u, Source: source, Width: w, Height: hgt,
			load: func(ctx context.Context) (image.Image, error) { return adimage.Fetch(ctx, h.Fetch, u) }})
	}
	faces := brandimages.FaceImages(t.Metadata)
	for _, img := range brandimages.OfKind(t.Metadata, t.ID, brandimages.KindPhoto) {
		if brandimages.NeverFrame(img) || faces[img.ID] || looksLikePoster(img) {
			continue
		}
		if l := h.Links(t.ID, []string{img.Key}); len(l) == 1 {
			add("brand-"+img.ID, l[0], "Your brand pictures", img.Width, img.Height)
		}
	}
	versions, err := h.DB.CreativeVersion.Query().
		Where(creativeversion.TenantIDEQ(t.ID), creativeversion.StatusEQ(creative.StatusApproved)).
		Order(ent.Desc(creativeversion.FieldCreatedAt)).Limit(10).All(ctx)
	if err != nil {
		return out
	}
	for _, v := range versions {
		// The consent covers the presenter in the video, not a still of it.
		if brandimages.VersionFace(v.Metadata) != "" || brandimages.CheckUse(ctx, h.DB, t.ID, v.ID) != nil {
			continue
		}
		assets, err := h.DB.CreativeAsset.Query().
			Where(creativeasset.TenantIDEQ(t.ID), creativeasset.VersionIDEQ(v.ID), creativeasset.PurgedAtIsNil(),
				creativeasset.KindIn(creative.AssetKeyframe, creative.AssetScene)).
			Order(ent.Asc(creativeasset.FieldCreatedAt)).All(ctx)
		if err != nil {
			continue
		}
		for _, a := range creative.CleanScenes(v, assets) {
			add("ad-"+a.ID.String(), a.URL, "Your approved ads", a.Width, a.Height)
		}
	}
	return out
}

// pictureName is the asset's name in Google Ads, which also says it is already there.
func pictureName(k materialKind, p Picture) string { return "MarketFlow " + p.ID + " " + k.Shape }

// pictureFields are the pictures offered for one shape that the group does not have.
func pictureFields(k materialKind, g google.AssetGroup, pics []Picture) []Field {
	var out []Field
	for _, p := range pics {
		if len(out) == pictureRows {
			break
		}
		if p.Width > 0 && p.Height > 0 {
			if w, h := adimage.Cut(p.Width, p.Height, k.Ratio); w < k.MinW || h < k.MinH {
				continue
			}
		}
		if g.Names[pictureName(k, p)] {
			continue
		}
		out = append(out, Field{Key: pictureKeyPrefix + k.Field + "|" + p.ID, Label: strings.ToUpper(k.Name[:1]) + k.Name[1:],
			Picture: &PictureChoice{URL: p.URL, Source: p.Source, Ratio: k.Ratio}})
	}
	return out
}

// pickedPictures cuts the ticked pictures.
func pickedPictures(ctx context.Context, v map[string]string, g google.AssetGroup, pics []Picture) ([]google.GroupAsset, error) {
	type pick struct {
		k materialKind
		p Picture
	}
	var picks []pick
	for _, k := range materialKinds {
		n := 0
		for _, p := range pics {
			if k.Ratio > 0 && v[pictureKeyPrefix+k.Field+"|"+p.ID] == "yes" {
				picks, n = append(picks, pick{k, p}), n+1
			}
		}
		if g.Count[k.Field]+n > k.Max && n > 0 {
			return nil, badInput("Google takes up to %d %ss in this ad group, and it has %d", k.Max, k.Name, g.Count[k.Field])
		}
	}
	if len(picks) > maxPictures {
		return nil, badInput("tick up to %d pictures at a time", maxPictures)
	}
	var out []google.GroupAsset
	for _, x := range picks {
		img, err := x.p.load(ctx)
		if err != nil {
			return nil, badInput("a picture you ticked could not be read; untick it or try again")
		}
		b, err := adimage.CropJPEG(img, x.k.Ratio, x.k.MinW, x.k.MinH)
		if err != nil {
			return nil, badInput("one %s you ticked is too small (Google needs %d by %d pixels)", x.k.Name, x.k.MinW, x.k.MinH)
		}
		out = append(out, google.GroupAsset{FieldType: x.k.Field, Name: pictureName(x.k, x.p), Image: b})
	}
	return out, nil
}

// lostPicture refuses a ticked picture that is not offered any more (removed, or now a consent picture).
func lostPicture(fields []Field, in map[string]string) error {
	offered := map[string]bool{}
	for _, f := range fields {
		offered[f.Key] = true
	}
	for k, v := range in {
		if strings.HasPrefix(k, pictureKeyPrefix) && strings.TrimSpace(v) != "" && !offered[k] {
			return badInput("a picture you ticked is not available any more; close this and open it again")
		}
	}
	return nil
}
