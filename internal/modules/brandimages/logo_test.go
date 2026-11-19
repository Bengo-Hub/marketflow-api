package brandimages

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// Logos never become video frames or poster references (2026-11-19 audit): a software house's
// site shows its clients' logos, and those must never reach that tenant's ads.
func TestLogosAreRecognised(t *testing.T) {
	for _, img := range []Image{{Logo: true}, {Name: "urban-loft-logo.png"}, {Source: "https://codevertex.example/clients/urban-loft.png"},
		{Caption: "Partner brand mark"}} {
		if !LooksLikeLogo(img) {
			t.Errorf("not seen as a logo: %+v", img)
		}
	}
	if LooksLikeLogo(Image{Name: "shop-front.jpg", Caption: "Our shop counter"}) {
		t.Error("a shop photo was taken for a logo")
	}

	page := `<html><img src="/a/shop.jpg" alt="Our shop"><img src="/b/urban.png" alt="The Urban Loft logo">
		<img src="/c/glow.jpg" class="clients-marquee__item"><img src="/d/team.jpg" alt="Team"></html>`
	got := ImageCandidates(page, "https://site.example/")
	if len(got) != 2 || got[0] != "https://site.example/a/shop.jpg" || got[1] != "https://site.example/d/team.jpg" {
		t.Fatalf("candidates: %v", got)
	}

	cut := image.NewNRGBA(image.Rect(0, 0, 40, 40)) // a mark on a see-through ground
	cut.Set(20, 20, color.NRGBA{200, 0, 0, 255})
	var buf bytes.Buffer
	_ = png.Encode(&buf, cut)
	if !mostlyTransparent(buf.Bytes()) {
		t.Fatal("a see-through picture is a logo")
	}
	solid := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	for i := range solid.Pix {
		solid.Pix[i] = 0xff
	}
	buf.Reset()
	_ = png.Encode(&buf, solid)
	if mostlyTransparent(buf.Bytes()) {
		t.Fatal("an opaque picture is not a logo")
	}
}
