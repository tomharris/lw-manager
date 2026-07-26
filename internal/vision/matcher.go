package vision

import (
	"fmt"
	"image"
	"math"

	"github.com/tomharris/lw-manager/internal/transport"
)

// MatchResult is the best template placement the matcher found: how good it is
// (Score, in [0,1]) and where it is, in resolution-independent coordinates so
// the result can be tapped or compared without ever naming a pixel upstream.
type MatchResult struct {
	Score  float64
	Center transport.Norm
	Box    transport.Rect
}

// Match slides tmpl across the search region of img and returns the placement
// with the highest normalized cross-correlation.
//
// It is deliberately a hand-rolled NCC rather than an OpenCV call: the no-CGO
// constraint rules out gocv, and it is feasible because anchors are small
// templates searched inside a restricted region, not a full-frame multi-scale
// hunt (M0 spec decision table).
//
// refHeight is the screen height the template library was captured at. The
// template is scaled by img.Height/refHeight before matching, so one template
// set serves every device resolution (design doc §3, resolution independence).
func Match(img image.Image, tmpl image.Image, region transport.Rect, refHeight int) (MatchResult, error) {
	if !region.Valid() {
		return MatchResult{}, fmt.Errorf("vision: search region %+v is not a valid unit-square rect", region)
	}
	if refHeight <= 0 {
		return MatchResult{}, fmt.Errorf("vision: refHeight must be positive, got %d", refHeight)
	}

	frame := Grayscale(img)
	tpl := Grayscale(tmpl)

	// Scale the template to this frame's resolution. The ratio is rarely a
	// whole number and is often < 1 (templates are usually captured at a
	// higher reference resolution than a given device), so resize to the exact
	// target dimensions rather than an integer upscale.
	scale := float64(frame.Bounds().Dy()) / float64(refHeight)
	if math.Abs(scale-1) > 1e-9 {
		tb := tpl.Bounds()
		nw := int(math.Round(float64(tb.Dx()) * scale))
		nh := int(math.Round(float64(tb.Dy()) * scale))
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
		tpl = resizeGray(tpl, nw, nh)
	}

	fb := frame.Bounds()
	tw, th := tpl.Bounds().Dx(), tpl.Bounds().Dy()
	if tw == 0 || th == 0 {
		return MatchResult{}, fmt.Errorf("vision: template has zero size")
	}
	if tw > fb.Dx() || th > fb.Dy() {
		return MatchResult{}, fmt.Errorf("vision: scaled template %dx%d larger than frame %dx%d", tw, th, fb.Dx(), fb.Dy())
	}

	// Precompute the template's mean and variance-magnitude once.
	tMean := meanGray(tpl, tpl.Bounds())
	var tVar float64
	tdev := make([]float64, tw*th)
	for j := 0; j < th; j++ {
		for i := 0; i < tw; i++ {
			d := float64(tpl.GrayAt(tpl.Bounds().Min.X+i, tpl.Bounds().Min.Y+j).Y) - tMean
			tdev[j*tw+i] = d
			tVar += d * d
		}
	}
	if tVar == 0 {
		return MatchResult{}, fmt.Errorf("vision: template has zero variance (uniform), NCC undefined")
	}

	// Search bounds: the region in frame pixels. Rounded with math.Round, the
	// same rule the template's own scaling uses above — mixing round() here
	// with truncation there is what made a region exactly the size of its
	// template shrink by a pixel relative to the scaled template and admit no
	// placement at all instead of one, at almost any scale factor.
	x1 := fb.Min.X + int(math.Round(region.X1*float64(fb.Dx())))
	y1 := fb.Min.Y + int(math.Round(region.Y1*float64(fb.Dy())))
	x2 := fb.Min.X + int(math.Round(region.X2*float64(fb.Dx())))
	y2 := fb.Min.Y + int(math.Round(region.Y2*float64(fb.Dy())))

	// The box's four corners are each rounded independently, so a region
	// defined to exactly match its own template's footprint can still land a
	// pixel short in either dimension purely from rounding, even with the
	// same rounding rule on both sides. Expand rather than trust the
	// arithmetic to land exactly: a search region is "where to look", which
	// by definition must never end up smaller than what it is looking for.
	if x2-x1 < tw {
		x2 = x1 + tw
	}
	if y2-y1 < th {
		y2 = y1 + th
	}
	// The expansion above can push x2/y2 past the frame's own far edge when
	// x1/y1 rounded toward it (a region flush against the right or bottom
	// edge, sized within a pixel of the scaled template). The placement loop
	// below is separately bounded by ox+tw<=fb.Max.X / oy+th<=fb.Max.Y and
	// has no way to claim pixels beyond that, so an unclamped x2 past
	// fb.Max.X silently shrinks the usable span below tw and the loop admits
	// nothing. Shift the whole box back by the overrun instead of clamping
	// only the far edge, so the span the loop actually gets to search keeps
	// its full tw/th width. Then clamp x1/y1 to fb.Min: for a template that
	// truly does not fit the frame (already rejected above by the
	// tw>fb.Dx()/th>fb.Dy() check) this is unreachable, but it keeps the
	// shift from ever producing a negative origin if that invariant is ever
	// weakened.
	if x2 > fb.Max.X {
		overrun := x2 - fb.Max.X
		x1 -= overrun
		x2 -= overrun
		if x1 < fb.Min.X {
			x1 = fb.Min.X
		}
	}
	if y2 > fb.Max.Y {
		overrun := y2 - fb.Max.Y
		y1 -= overrun
		y2 -= overrun
		if y1 < fb.Min.Y {
			y1 = fb.Min.Y
		}
	}

	best := MatchResult{Score: -2} // below any real NCC, which lives in [-1,1]
	found := false
	for oy := y1; oy+th <= y2 && oy+th <= fb.Max.Y; oy++ {
		for ox := x1; ox+tw <= x2 && ox+tw <= fb.Max.X; ox++ {
			found = true
			score := ncc(frame, ox, oy, tdev, tVar, tw, th)
			if score > best.Score {
				best.Score = score
				// Normalize relative to the frame's own origin: fb.Min is
				// non-zero when img is a cropped sub-image, and without
				// subtracting it the coordinates drift past [0,1].
				cx := float64(ox+tw/2-fb.Min.X) / float64(fb.Dx())
				cy := float64(oy+th/2-fb.Min.Y) / float64(fb.Dy())
				best.Center = transport.Norm{X: cx, Y: cy}
				best.Box = transport.Rect{
					X1: float64(ox-fb.Min.X) / float64(fb.Dx()),
					Y1: float64(oy-fb.Min.Y) / float64(fb.Dy()),
					X2: float64(ox+tw-fb.Min.X) / float64(fb.Dx()),
					Y2: float64(oy+th-fb.Min.Y) / float64(fb.Dy()),
				}
			}
		}
	}
	if !found {
		return MatchResult{}, fmt.Errorf("vision: search region admits no placement for a %dx%d template", tw, th)
	}
	// Clamp to [0,1]: negative correlation is anti-match, which upstream reads
	// as simply "not found".
	if best.Score < 0 {
		best.Score = 0
	}
	return best, nil
}

// ncc computes normalized cross-correlation of the template deviations against
// the frame patch anchored at (ox,oy). Returns a value in [-1,1].
func ncc(frame *image.Gray, ox, oy int, tdev []float64, tVar float64, tw, th int) float64 {
	// Patch mean.
	var sum float64
	for j := 0; j < th; j++ {
		for i := 0; i < tw; i++ {
			sum += float64(frame.GrayAt(ox+i, oy+j).Y)
		}
	}
	mean := sum / float64(tw*th)

	var num, pVar float64
	for j := 0; j < th; j++ {
		for i := 0; i < tw; i++ {
			d := float64(frame.GrayAt(ox+i, oy+j).Y) - mean
			num += d * tdev[j*tw+i]
			pVar += d * d
		}
	}
	if pVar == 0 {
		return 0 // flat patch: no correlation possible
	}
	return num / math.Sqrt(tVar*pVar)
}

// Resize scales an image to exact dimensions, in grayscale.
//
// Exported so the scoring harness can synthesize a second resolution from a
// one-handset corpus. That measures the matcher's scale handling; it is not
// evidence of cross-device generalization, because a real second device
// differs in DPI and therefore in layout and font hinting, not only in scale.
func Resize(img image.Image, w, h int) *image.Gray {
	return resizeGray(Grayscale(img), w, h)
}

// resizeGray nearest-neighbour resizes src to w×h. Nearest neighbour keeps the
// template's hard edges (and matches Upscale for integer factors), which is
// what NCC correlates against; interpolation would blur the very structure the
// match depends on.
func resizeGray(src *image.Gray, w, h int) *image.Gray {
	sb := src.Bounds()
	out := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := sb.Min.Y + y*sb.Dy()/h
		for x := 0; x < w; x++ {
			sx := sb.Min.X + x*sb.Dx()/w
			out.SetGray(x, y, src.GrayAt(sx, sy))
		}
	}
	return out
}

// variance returns the sum of squared deviations from the mean gray value.
// Zero means every pixel is identical: NCC is undefined against such a
// template (see the tVar check in Match), and it is exactly what a crop of
// blank UI produces — a perfectly valid PNG with nothing in it to look for.
func variance(img image.Image) float64 {
	g := Grayscale(img)
	b := g.Bounds()
	mean := meanGray(g, b)
	var v float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			d := float64(g.GrayAt(x, y).Y) - mean
			v += d * d
		}
	}
	return v
}

func meanGray(g *image.Gray, b image.Rectangle) float64 {
	var sum float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			sum += float64(g.GrayAt(x, y).Y)
		}
	}
	return sum / float64(b.Dx()*b.Dy())
}
