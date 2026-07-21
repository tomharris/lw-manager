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

	// Scale the template to this frame's resolution.
	scale := float64(frame.Bounds().Dy()) / float64(refHeight)
	if math.Abs(scale-1) > 1e-9 {
		f := int(math.Round(scale))
		if f < 1 {
			f = 1
		}
		tpl = Upscale(tpl, f)
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

	// Search bounds: the region in frame pixels, clamped so the template fits.
	x1 := fb.Min.X + int(region.X1*float64(fb.Dx()))
	y1 := fb.Min.Y + int(region.Y1*float64(fb.Dy()))
	x2 := fb.Min.X + int(region.X2*float64(fb.Dx()))
	y2 := fb.Min.Y + int(region.Y2*float64(fb.Dy()))

	best := MatchResult{Score: -2} // below any real NCC, which lives in [-1,1]
	found := false
	for oy := y1; oy+th <= y2 && oy+th <= fb.Max.Y; oy++ {
		for ox := x1; ox+tw <= x2 && ox+tw <= fb.Max.X; ox++ {
			found = true
			score := ncc(frame, ox, oy, tdev, tVar, tw, th)
			if score > best.Score {
				best.Score = score
				cx := float64(ox+tw/2) / float64(fb.Dx())
				cy := float64(oy+th/2) / float64(fb.Dy())
				best.Center = transport.Norm{X: cx, Y: cy}
				best.Box = transport.Rect{
					X1: float64(ox) / float64(fb.Dx()),
					Y1: float64(oy) / float64(fb.Dy()),
					X2: float64(ox+tw) / float64(fb.Dx()),
					Y2: float64(oy+th) / float64(fb.Dy()),
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

func meanGray(g *image.Gray, b image.Rectangle) float64 {
	var sum float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			sum += float64(g.GrayAt(x, y).Y)
		}
	}
	return sum / float64(b.Dx()*b.Dy())
}
