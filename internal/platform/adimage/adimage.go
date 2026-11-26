// Package adimage cuts a business's pictures to the shapes ad platforms take.
package adimage

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif" // decoders
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// maxPixels refuses a picture that would not fit in memory.
const maxPixels = 60_000_000

// Fetch downloads a picture.
func Fetch(ctx context.Context, hc *http.Client, src string) (image.Image, error) {
	if src == "" {
		return nil, fmt.Errorf("no image")
	}
	if hc == nil {
		return nil, fmt.Errorf("images cannot be fetched on this server")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the image could not be downloaded")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the image could not be downloaded")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 12<<20))
	if err != nil {
		return nil, fmt.Errorf("the image could not be downloaded")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width*cfg.Height > maxPixels {
		return nil, fmt.Errorf("the image format is not readable")
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("the image format is not readable")
	}
	return img, nil
}

// Cut is the size of the centre crop of a w by h picture at ratio (width/height).
func Cut(w, h int, ratio float64) (int, int) {
	cw, ch := w, int(float64(w)/ratio)
	if ch > h {
		ch, cw = h, int(float64(h)*ratio)
	}
	return cw, ch
}

// CropJPEG is the centre crop of img at ratio, scaled to at most 1200 wide, as JPEG.
func CropJPEG(img image.Image, ratio float64, minW, minH int) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	cw, ch := Cut(w, h, ratio)
	if cw < minW || ch < minH {
		return nil, fmt.Errorf("the image is too small (needs %d by %d pixels)", minW, minH)
	}
	x0, y0 := b.Min.X+(w-cw)/2, b.Min.Y+(h-ch)/2
	crop := image.Rect(x0, y0, x0+cw, y0+ch)
	outW, outH := cw, ch
	if outW > 1200 {
		outW, outH = 1200, int(1200/ratio)
	}
	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, crop, xdraw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
