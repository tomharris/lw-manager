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
// adaptive threshold → invert → upscale. That recipe's claimed accuracy does
// not hold on this game's UI: measured against the first real capture
// (docs/superpowers/specs/evidence/m4-ocr-2026-08-14), the full chain read a
// clean header crop as "(es Thisisit CED 4]" — adaptive thresholding
// normalizes local contrast, so on a nearly-flat region (a header's flat
// background, a card's flat fill) it invents edges out of noise instead of
// finding real ones. This is the same trap CLAUDE.md documents for NCC
// template matching on flat crops, in a different algorithm: a normalizing
// step amplifies whatever variance is there, real or not.
//
// What replaced "the recipe is load-bearing" is per-field measurement
// (internal/vision/zz_preproc_probe_test.go's TestPreprocMeasure, run
// against real frames): every field internal/ingest reads was fastest and
// most accurate with grayscale-and-upscale alone, threshold and equalize
// included or not depending on the field. Preprocess's own default (Options{})
// still runs the full chain unchanged for any caller that does not opt out —
// callers may depend on it, and this file changes no behavior, only the
// claim about why it works. internal/ingest's readField is the caller that
// now opts out, per field, with the measured Options recorded beside each
// field's ocr.Spec in roster.go and vs.go.
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
// upscale when Options is the zero value. That full chain is kept as the
// default for backward compatibility, not because it is generally correct —
// see the package doc comment: on this game's UI, threshold and equalize
// are each wrong often enough that every field internal/ingest reads now
// opts out of at least one of them via Options, measured per field rather
// than assumed. Steps can be skipped via Options for callers that only need
// part.
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

// GreenChannel presents an image's green channel as a grayscale image, so a
// caller that hands the result to Preprocess gets that channel instead of luma
// (Preprocess's own Grayscale step is then a no-op).
//
// It exists for one measured field. The member list's status column renders
// two states -- a grey elapsed time and the word "Online" in green with a dark
// outline -- and luma (0.299R + 0.587G + 0.114B) leaves that green sitting
// close to the cream card behind it, so the glyphs read as a hollow outline
// and tesseract returns garbage. Measured over capture 1's 277 attributable
// row bands (`make probe-roster PROBE_ARGS=-roster.lastactive`), the shipped
// luma path parses 7 of 24 Online rows; the green channel parses 12.
//
// Why green and not the red channel the luma weights would suggest: the
// argument from weights predicts red should separate green text from a cream
// background best, and the measurement says red parses 0 of 24. That is the
// whole reason this function exists behind a probe mode rather than behind a
// derivation -- the sweep is in zz_roster_probe_test.go and the grid it ran is
// 144 shape/PSM combinations.
//
// It is not a general improvement and must not be applied as one: the same
// measurement has the green channel parsing 202 of 253 grey elapsed-time rows
// against luma's 250. It is a RETRY for a read luma could not parse, never a
// replacement for luma.
func GreenChannel(img image.Image) image.Image { return greenChannel{img} }

type greenChannel struct{ src image.Image }

func (c greenChannel) ColorModel() color.Model { return color.GrayModel }
func (c greenChannel) Bounds() image.Rectangle { return c.src.Bounds() }
func (c greenChannel) At(x, y int) color.Color {
	_, g, _, _ := c.src.At(x, y).RGBA()
	return color.Gray{Y: uint8(g >> 8)}
}
