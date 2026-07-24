// Package vision turns a screenshot into facts a task can act on: where an
// anchor is (matcher), which screen we are looking at (recognizer), and what a
// region of pixels says (the ocr package consumes this one's output).
//
// Everything here is pure image math on image.Image, with no device and no
// external process, so invariant #6 holds: all vision logic ships with
// fixture-based tests that run with no device attached. The one exception —
// the real Tesseract subprocess — lives in the ocr package and skips when the
// binary is absent.
//
// The preprocessing pipeline is the reference alliance-manager's recipe
// (docs/superpowers/specs M0 §Corrections): crop → grayscale → equalize →
// adaptive threshold → invert → upscale. Its accuracy came from preprocessing
// and validation, not the OCR engine, so these steps are load-bearing.
package vision

import (
	"image"
	"image/color"

	"github.com/tomharris/lw-manager/internal/transport"
)

// Equalize spreads a gray image's histogram across the full [0,255] range.
// Screenshot text is often stylized and low-contrast; equalization is what
// makes the later threshold cut cleanly rather than swallowing faint pixels.
func Equalize(img *image.Gray) *image.Gray {
	b := img.Bounds()

	var hist [256]int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			hist[img.GrayAt(x, y).Y]++
		}
	}

	// Cumulative distribution, and its smallest non-zero value: the darkest
	// occupied level must map to 0 for the stretch to reach the full range.
	var cdf [256]int
	var lut [256]uint8
	cum, cdfMin := 0, 0
	total := b.Dx() * b.Dy()
	for i := 0; i < 256; i++ {
		cum += hist[i]
		cdf[i] = cum
		if cdfMin == 0 && cum > 0 {
			cdfMin = cum
		}
	}
	denom := total - cdfMin
	for i := 0; i < 256; i++ {
		switch {
		case hist[i] == 0 || denom <= 0:
			lut[i] = uint8(i)
		default:
			lut[i] = uint8((cdf[i] - cdfMin) * 255 / denom)
		}
	}

	out := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.SetGray(x, y, color.Gray{Y: lut[img.GrayAt(x, y).Y]})
		}
	}
	return out
}

// AdaptiveThreshold binarizes by comparing each pixel to the mean of the
// block×block window around it, minus c. A pixel brighter than its local mean
// becomes 255, else 0. Local means, not a global cut, are what let faint text
// survive next to a bright HUD element on the same frame.
func AdaptiveThreshold(img *image.Gray, block, c int) *image.Gray {
	if block < 1 {
		block = 1
	}
	b := img.Bounds()
	r := block / 2
	out := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			sum, n := 0, 0
			for wy := y - r; wy <= y+r; wy++ {
				for wx := x - r; wx <= x+r; wx++ {
					if wx < b.Min.X || wx >= b.Max.X || wy < b.Min.Y || wy >= b.Max.Y {
						continue
					}
					sum += int(img.GrayAt(wx, wy).Y)
					n++
				}
			}
			mean := sum / n
			if int(img.GrayAt(x, y).Y) > mean-c {
				out.SetGray(x, y, color.Gray{Y: 255})
			} else {
				out.SetGray(x, y, color.Gray{Y: 0})
			}
		}
	}
	return out
}

// Invert flips a gray image. Tesseract expects dark text on a light ground;
// game screens are usually the reverse, so binarize then invert.
func Invert(img *image.Gray) *image.Gray {
	b := img.Bounds()
	out := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.SetGray(x, y, color.Gray{Y: 255 - img.GrayAt(x, y).Y})
		}
	}
	return out
}

// Upscale enlarges by an integer factor using nearest-neighbour sampling.
// OCR accuracy climbs sharply with size on small numerals, and nearest
// neighbour keeps the hard binary edges the threshold just produced instead of
// blurring them the way interpolation would.
func Upscale(img *image.Gray, factor int) *image.Gray {
	if factor < 1 {
		factor = 1
	}
	b := img.Bounds()
	out := image.NewGray(image.Rect(0, 0, b.Dx()*factor, b.Dy()*factor))
	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := 0; x < out.Bounds().Dx(); x++ {
			src := img.GrayAt(b.Min.X+x/factor, b.Min.Y+y/factor)
			out.SetGray(x, y, src)
		}
	}
	return out
}

// Options tunes the pipeline. Zero values are filled with the reference
// alliance-manager defaults, so Preprocess(img, Options{}) is the sane path
// and callers override only what a field demands (dense rows want a tighter
// threshold block, per M0 §Corrections).
type Options struct {
	Region         transport.Rect // zero rect means the whole frame
	ThresholdBlock int            // odd; default 25
	ThresholdC     int            // default 10
	UpscaleFactor  int            // default 3
	SkipEqualize   bool
	SkipThreshold  bool
	SkipInvert     bool
}

// Preprocess runs crop → grayscale → equalize → adaptive threshold → invert →
// upscale, the recipe whose accuracy came from these steps rather than the OCR
// engine. Steps can be skipped via Options for callers that only need part.
func Preprocess(img image.Image, opts Options) *image.Gray {
	if opts.ThresholdBlock == 0 {
		opts.ThresholdBlock = 25
	}
	if opts.ThresholdC == 0 {
		opts.ThresholdC = 10
	}
	if opts.UpscaleFactor == 0 {
		opts.UpscaleFactor = 3
	}

	src := img
	if opts.Region.Valid() {
		src = Crop(img, opts.Region)
	}
	g := Grayscale(src)
	if !opts.SkipEqualize {
		g = Equalize(g)
	}
	if !opts.SkipThreshold {
		g = AdaptiveThreshold(g, opts.ThresholdBlock, opts.ThresholdC)
	}
	if !opts.SkipInvert {
		g = Invert(g)
	}
	return Upscale(g, opts.UpscaleFactor)
}

// subImager is implemented by the stdlib image types, which can hand back a
// view onto a sub-rectangle without copying pixels.
type subImager interface {
	SubImage(r image.Rectangle) image.Image
}

// Crop returns the region of img described by a normalized rectangle. It is
// the pipeline's first step: cut to the data region and drop the surrounding
// UI chrome before any per-pixel work. The result keeps the source's pixel
// offset (it is a view, not a re-origined copy) so a match found in a crop
// maps straight back to the full frame.
func Crop(img image.Image, r transport.Rect) image.Image {
	b := img.Bounds()
	rect := image.Rect(
		b.Min.X+int(r.X1*float64(b.Dx())),
		b.Min.Y+int(r.Y1*float64(b.Dy())),
		b.Min.X+int(r.X2*float64(b.Dx())),
		b.Min.Y+int(r.Y2*float64(b.Dy())),
	).Intersect(b)

	if si, ok := img.(subImager); ok {
		return si.SubImage(rect)
	}
	// Fallback for image types without SubImage: copy into a fresh gray view.
	out := image.NewGray(rect)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out
}

// Grayscale converts any image to 8-bit gray using Go's luminance weighting.
// Every later step operates on a single channel, so this is the pipeline's
// entry point.
func Grayscale(img image.Image) *image.Gray {
	b := img.Bounds()
	g := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			g.Set(x, y, color.GrayModel.Convert(img.At(x, y)))
		}
	}
	return g
}
