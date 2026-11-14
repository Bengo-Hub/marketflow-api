package brandimages

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // decoders for website pictures

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // websites often serve WebP
)

// Pictures linked from a website are resized to what an upload must be (owner, 2026-11-14), the
// same rule as the portal's uploader: a short side below the minimum is enlarged up to maxUpscale
// times, a long side above maxLongSide is reduced (never below the minimum short side), and the
// result is a JPEG under the size cap.

const (
	maxUpscale  = 2.0
	maxLongSide = 2560
)

// jpegQualities are tried best first until the file fits under the cap with some headroom.
var jpegQualities = []int{90, 85, 80, 72, 65}

// errFarTooSmall is a picture too small to enlarge well.
var errFarTooSmall = errors.New("picture too small to enlarge well")

// fitSize returns the size a picture is resized to and whether it changes.
func fitSize(w, h, minShort int) (int, int, bool, error) {
	short, long := min(w, h), max(w, h)
	scale := 1.0
	switch {
	case short < minShort:
		scale = float64(minShort) / float64(short)
		if scale > maxUpscale {
			return w, h, false, errFarTooSmall
		}
	case long > maxLongSide:
		scale = max(float64(maxLongSide)/float64(long), float64(minShort)/float64(short))
	}
	if scale == 1 {
		return w, h, false, nil
	}
	nw, nh := int(float64(w)*scale+0.5), int(float64(h)*scale+0.5)
	if w <= h && nw < minShort {
		nw = minShort
	}
	if h < w && nh < minShort {
		nh = minShort
	}
	return nw, nh, true, nil
}

// fitPicture returns the picture ready to store: unchanged when it is already a JPEG or PNG of the
// right size under the cap, else resized and re-encoded as JPEG.
func (s *Service) fitPicture(raw []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	nw, nh, resize, err := fitSize(cfg.Width, cfg.Height, s.cfg.MinShortSide)
	if err != nil {
		return nil, err
	}
	budget := int(float64(s.cfg.MaxBytes) * 0.9)
	if !resize && (format == "jpeg" || format == "png") && len(raw) <= budget {
		return raw, nil
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	// A transparent picture gets a white background rather than black.
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	var out []byte
	for _, q := range jpegQualities {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: q}); err != nil {
			return nil, err
		}
		out = buf.Bytes()
		if len(out) <= budget {
			break
		}
	}
	return out, nil
}
