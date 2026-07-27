package vision

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// distinctTemplate builds a w×h gray image with a varied, non-uniform pattern.
// NCC is undefined on a flat patch (zero variance), so an anchor fixture must
// actually have structure — this is the smallest thing that does.
func distinctTemplate(w, h int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g.SetGray(x, y, color.Gray{Y: uint8((x*37 + y*53) % 256)})
		}
	}
	return g
}

// paste copies src onto a fresh flat-gray background of size w×h at (px,py).
func paste(w, h int, bg uint8, src *image.Gray, px, py int) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out.SetGray(x, y, color.Gray{Y: bg})
		}
	}
	sb := src.Bounds()
	for y := 0; y < sb.Dy(); y++ {
		for x := 0; x < sb.Dx(); x++ {
			out.SetGray(px+x, py+y, src.GrayAt(sb.Min.X+x, sb.Min.Y+y))
		}
	}
	return out
}

func TestMatchFindsTemplateAtKnownLocation(t *testing.T) {
	tmpl := distinctTemplate(8, 8)
	frame := paste(64, 64, 100, tmpl, 20, 30) // template top-left at (20,30)

	m, err := Match(frame, tmpl, transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, 64)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if m.Score < 0.99 {
		t.Errorf("score: got %.4f, want ≥0.99", m.Score)
	}
	// Center of an 8×8 template placed at (20,30) is pixel (24,34) → normalized.
	wantX, wantY := 24.0/64, 34.0/64
	if math.Abs(m.Center.X-wantX) > 0.02 || math.Abs(m.Center.Y-wantY) > 0.02 {
		t.Errorf("center: got (%.4f,%.4f), want ~(%.4f,%.4f)", m.Center.X, m.Center.Y, wantX, wantY)
	}
}

func TestMatchScoresLowWhenAbsent(t *testing.T) {
	tmpl := distinctTemplate(8, 8)
	// A frame with a *different* pattern where the template never appears.
	frame := paste(64, 64, 100, distinctTemplate(8, 8), 200, 200) // paste off-canvas: no-op
	// Overwrite with a smooth gradient so there is genuinely no match.
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			frame.SetGray(x, y, color.Gray{Y: uint8((x + y) % 256)})
		}
	}

	m, err := Match(frame, tmpl, transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, 64)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if m.Score > 0.95 {
		t.Errorf("absent template should score low, got %.4f", m.Score)
	}
}

func TestMatchScalesByFractionAndDownscale(t *testing.T) {
	tmpl := distinctTemplate(16, 16)

	cases := []struct {
		name      string
		frameH    int
		refHeight int
		newSize   int // expected scaled template edge
	}{
		{"upscale 1.5x", 96, 64, 24},
		{"downscale 0.5x", 64, 128, 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scaled := resizeGray(Grayscale(tmpl), c.newSize, c.newSize)
			frame := paste(c.frameH, c.frameH, 100, scaled, 20, 20)

			m, err := Match(frame, tmpl, transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, c.refHeight)
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if m.Score < 0.99 {
				t.Errorf("score: got %.4f, want ≥0.99", m.Score)
			}
			wantX := (20.0 + float64(c.newSize)/2) / float64(c.frameH)
			if math.Abs(m.Center.X-wantX) > 0.03 {
				t.Errorf("center X: got %.4f, want ~%.4f", m.Center.X, wantX)
			}
		})
	}
}

func TestMatchOnCroppedSubImageReturnsValidNorm(t *testing.T) {
	tmpl := distinctTemplate(8, 8)
	full := paste(64, 64, 100, tmpl, 40, 20) // template at absolute (40,20)
	crop := Crop(full, transport.Rect{X1: 0.5, Y1: 0, X2: 1, Y2: 1})

	m, err := Match(crop, tmpl, transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, 64)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !m.Center.Valid() || !m.Box.Valid() {
		t.Fatalf("match on crop produced out-of-range coords: center=%+v box=%+v", m.Center, m.Box)
	}
	// In the crop's own frame (x 32..64), the template's center pixel (44) is
	// at (44-32)/32 = 0.375.
	if math.Abs(m.Center.X-0.375) > 0.03 {
		t.Errorf("center X within crop: got %.4f, want ~0.375", m.Center.X)
	}
}

// TestMatchExactFitSearchRegionAdmitsPlacementAcrossScales reproduces the
// studio's crop bug directly at the matcher level: a search region that is
// exactly the size of the template it bounds, at the reference resolution
// (the shape the studio wrote before it started padding the stored region —
// see handleCrop). Before this test's fix, the template was scaled with
// math.Round while the search box was scaled with int() truncation, and even
// after unifying the rounding, each of the box's four corners still rounds
// independently of the template's one whole-dimension scale — either way, a
// one-pixel disagreement made the placement loop admit no candidate at all,
// and Match returned "search region admits no placement" instead of a score.
// That is exactly what broke `agent score --rescale 0.75,1.25`, the design
// doc's own resolution-independence check.
func TestMatchExactFitSearchRegionAdmitsPlacementAcrossScales(t *testing.T) {
	const refHeight = 240
	const tw, th = 80, 60
	const ox, oy = 60, 90 // template's top-left within a refHeight x refHeight reference frame
	tmpl := distinctTemplate(tw, th)
	region := transport.Rect{
		X1: float64(ox) / refHeight, Y1: float64(oy) / refHeight,
		X2: float64(ox+tw) / refHeight, Y2: float64(oy+th) / refHeight,
	}

	for _, scale := range []float64{0.75, 0.9, 1.0, 1.25} {
		t.Run(fmt.Sprintf("scale=%.2f", scale), func(t *testing.T) {
			frameH := int(math.Round(refHeight * scale))
			nw := int(math.Round(tw * scale))
			nh := int(math.Round(th * scale))
			scaledTmpl := resizeGray(Grayscale(tmpl), nw, nh)
			px := int(math.Round(float64(ox) * scale))
			py := int(math.Round(float64(oy) * scale))
			frame := paste(frameH, frameH, 100, scaledTmpl, px, py)

			m, err := Match(frame, tmpl, region, refHeight)
			if err != nil {
				t.Fatalf("Match at scale %.2f: %v (an exactly-fitting region must still admit one placement)", scale, err)
			}
			if m.Score < 0.99 {
				t.Errorf("score at scale %.2f: got %.4f, want >=0.99", scale, m.Score)
			}
		})
	}
}

// TestMatchFlushRegionAdmitsPlacementAcrossScales reproduces the flush-edge
// regression: after unifying rounding (see the comment on the search-bounds
// block in Match), a region sized to exactly match its template can have
// math.Round move x1/y1 one pixel toward the far edge. The expand step
// ("a search region... must never end up smaller than what it is looking
// for") then pushes x2/y2 past fb.Max, and the placement loop is separately
// bounded by ox+tw<=fb.Max.X / oy+th<=fb.Max.Y, so it can never give that
// pixel back — for a region flush against the frame's right or bottom edge,
// the loop admits nothing and Match errors instead of scoring.
//
// Frame and template sizes mirror the reviewer's repro: a 200x300 frame,
// 20x30 template, refHeight 300. Before the fix this fails at every
// half-pixel scale (0.75, 0.85, 0.95, 1.05, 1.15, 1.25); whole/quarter
// scales (0.70, 0.80, ... 1.50) happen not to trigger the off-by-one and
// already pass, which is why the table matters more than a single case.
//
// The 200x300/20x30 pairing only ever puts the rounding disagreement on the
// Y axis: 200*scale is a multiple of 10 for every scale in the table, so
// region.X1*frameWidth always lands on a whole pixel and x1/x2 need no
// expansion, while 300*scale is not, so the Y arithmetic is the one that
// drifts. "right-edge-only" therefore swaps width/height and template
// dimensions (refW=300, refH=200, tw=30, th=20) so the X axis inherits the
// same "not a multiple of 10" arithmetic and actually exercises the x1/x2
// shift — with the original orientation it would silently pass before the
// fix and prove nothing.
func TestMatchFlushRegionAdmitsPlacementAcrossScales(t *testing.T) {
	scales := []float64{0.70, 0.75, 0.80, 0.85, 0.90, 0.95, 1.00, 1.05, 1.10, 1.15, 1.20, 1.25, 1.50}

	corners := []struct {
		name       string
		refW, refH int
		tw, th     int
		ox, oy     int // template's top-left in the refW x refH reference frame
	}{
		{"bottom-right", 200, 300, 20, 30, 200 - 20, 300 - 30},
		{"right-edge-only", 300, 200, 30, 20, 300 - 30, (200 - 20) / 2},
		{"bottom-edge-only", 200, 300, 20, 30, (200 - 20) / 2, 300 - 30},
	}

	for _, c := range corners {
		t.Run(c.name, func(t *testing.T) {
			refW, refH, tw, th := c.refW, c.refH, c.tw, c.th
			tmpl := distinctTemplate(tw, th)
			region := transport.Rect{
				X1: float64(c.ox) / float64(refW), Y1: float64(c.oy) / float64(refH),
				X2: float64(c.ox+tw) / float64(refW), Y2: float64(c.oy+th) / float64(refH),
			}
			for _, scale := range scales {
				t.Run(fmt.Sprintf("scale=%.2f", scale), func(t *testing.T) {
					frameW := int(math.Round(float64(refW) * scale))
					frameH := int(math.Round(float64(refH) * scale))
					nw := int(math.Round(float64(tw) * scale))
					nh := int(math.Round(float64(th) * scale))
					if nw < 1 {
						nw = 1
					}
					if nh < 1 {
						nh = 1
					}
					scaledTmpl := resizeGray(Grayscale(tmpl), nw, nh)
					px := int(math.Round(float64(c.ox) * scale))
					py := int(math.Round(float64(c.oy) * scale))
					// Keep the pasted copy inside the frame even if rounding
					// nudged it past the edge, mirroring how a real capture
					// would never overflow its own canvas.
					if px+nw > frameW {
						px = frameW - nw
					}
					if py+nh > frameH {
						py = frameH - nh
					}
					frame := paste(frameW, frameH, 100, scaledTmpl, px, py)

					m, err := Match(frame, tmpl, region, refH)
					if err != nil {
						t.Fatalf("Match at scale %.2f: %v (a region flush to the frame edge, sized to its template, must still admit a placement)", scale, err)
					}
					if m.Score < 0 || m.Score > 1 {
						t.Errorf("score out of range at scale %.2f: %.4f", scale, m.Score)
					}
				})
			}
		})
	}
}

// TestMatchTopLeftFlushRegionOriginNotBelowMin confirms the fix's shift never
// walks x1/y1 below fb.Min: a region flush to the top-left never needs the
// far-edge expansion to overrun in the first place (there is nothing beyond
// fb.Max to push into), so this path should be untouched by the fix, but it
// is exactly the case the clamp step must not break.
func TestMatchTopLeftFlushRegionOriginNotBelowMin(t *testing.T) {
	const refW, refH = 200, 300
	const tw, th = 20, 30
	tmpl := distinctTemplate(tw, th)

	for _, scale := range []float64{0.70, 0.85, 1.00, 1.15, 1.50} {
		t.Run(fmt.Sprintf("scale=%.2f", scale), func(t *testing.T) {
			region := transport.Rect{X1: 0, Y1: 0, X2: float64(tw) / refW, Y2: float64(th) / refH}
			frameW := int(math.Round(float64(refW) * scale))
			frameH := int(math.Round(float64(refH) * scale))
			nw := int(math.Round(float64(tw) * scale))
			nh := int(math.Round(float64(th) * scale))
			scaledTmpl := resizeGray(Grayscale(tmpl), nw, nh)
			frame := paste(frameW, frameH, 100, scaledTmpl, 0, 0)

			m, err := Match(frame, tmpl, region, refH)
			if err != nil {
				t.Fatalf("Match at scale %.2f: %v", scale, err)
			}
			if m.Box.X1 < 0 || m.Box.Y1 < 0 {
				t.Errorf("box origin below frame min at scale %.2f: %+v", scale, m.Box)
			}
		})
	}
}

// TestMatchTemplateLargerThanFrameStillFails confirms the fix's shift-and-
// clamp does not paper over a template that plainly cannot fit: this trips
// the pre-existing "scaled template larger than frame" guard at the top of
// Match, before search bounds (and therefore the shift) are ever computed,
// so it must keep failing exactly as it did before this change.
func TestMatchTemplateLargerThanFrameStillFails(t *testing.T) {
	tmpl := distinctTemplate(50, 50)
	frame := paste(40, 40, 100, distinctTemplate(40, 40), 0, 0)

	_, err := Match(frame, tmpl, transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, 40)
	if err == nil {
		t.Fatal("Match: expected an error for a template larger than the frame, got nil")
	}
}

// TestMatchMidFrameRegionScoreUnchanged pins the score for a region that sits
// well clear of every edge (5x and 6x the template's width/height of slack in
// x and y respectively), so the expand-and-shift path is never entered. This
// must produce the same result before and after the fix.
func TestMatchMidFrameRegionScoreUnchanged(t *testing.T) {
	tmpl := distinctTemplate(8, 8)
	frame := paste(64, 64, 100, tmpl, 20, 30)

	m, err := Match(frame, tmpl, transport.Rect{X1: 0.2, Y1: 0.3, X2: 0.6, Y2: 0.7}, 64)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	const wantScore = 1.0
	if math.Abs(m.Score-wantScore) > 1e-9 {
		t.Errorf("score: got %.10f, want %.10f", m.Score, wantScore)
	}
}

func TestMatchIsScaleInvariant(t *testing.T) {
	tmpl := distinctTemplate(8, 8)
	big := Upscale(tmpl, 2) // how the anchor appears at 2× res
	frame := paste(128, 128, 100, big, 40, 50)

	// refHeight 64, actual height 128 → matcher must scale the template ×2.
	m, err := Match(frame, tmpl, transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, 64)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if m.Score < 0.99 {
		t.Errorf("scaled match score: got %.4f, want ≥0.99", m.Score)
	}
	// 16×16 template at (40,50) → center pixel (48,58).
	wantX, wantY := 48.0/128, 58.0/128
	if math.Abs(m.Center.X-wantX) > 0.02 || math.Abs(m.Center.Y-wantY) > 0.02 {
		t.Errorf("scaled center: got (%.4f,%.4f), want ~(%.4f,%.4f)", m.Center.X, m.Center.Y, wantX, wantY)
	}
}
