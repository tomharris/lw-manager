package vision

import (
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
