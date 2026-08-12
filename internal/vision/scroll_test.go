package vision

import (
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// stripedFrame draws horizontal bands of varying grey so a vertical shift is
// unambiguous. A flat image would correlate with itself at every offset, which
// is the degenerate case ScrollOffset must not be tested against.
func stripedFrame(w, h, shift int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		// +shift moves content up: the band that was at y+shift is now at y.
		v := uint8((((y + shift) / 7) * 37) % 251)
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: v, G: v, B: uint8((x / 5 * 11) % 251), A: 255})
		}
	}
	return img
}

func TestScrollOffsetMeasuresAKnownShift(t *testing.T) {
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8}
	prev := stripedFrame(120, 800, 0)
	cur := stripedFrame(120, 800, 64)

	got, err := ScrollOffset(prev, cur, region)
	if err != nil {
		t.Fatalf("ScrollOffset: %v", err)
	}
	if got != 64 {
		t.Fatalf("offset = %d, want 64", got)
	}
}

func TestScrollOffsetReturnsZeroWhenNothingMoved(t *testing.T) {
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8}
	frame := stripedFrame(120, 800, 0)

	got, err := ScrollOffset(frame, frame, region)
	if err != nil {
		t.Fatalf("ScrollOffset: %v", err)
	}
	if got != 0 {
		t.Fatalf("offset = %d, want 0", got)
	}
}

// A change outside the region must not register as scrolling. This is the
// announcement banner recon caught animating in the header: while it runs,
// whole-frame comparison reports progress forever.
func TestScrollOffsetIgnoresChangeOutsideTheRegion(t *testing.T) {
	region := transport.Rect{X1: 0, Y1: 0.5, X2: 1, Y2: 0.9}
	prev := stripedFrame(120, 800, 0)
	cur := stripedFrame(120, 800, 0)
	for y := 0; y < 200; y++ {
		for x := 0; x < 120; x++ {
			cur.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}

	got, err := ScrollOffset(prev, cur, region)
	if err != nil {
		t.Fatalf("ScrollOffset: %v", err)
	}
	if got != 0 {
		t.Fatalf("offset = %d, want 0 — change outside the region must not count", got)
	}
}

func TestScrollOffsetRejectsAFlatRegion(t *testing.T) {
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8}
	flat := image.NewRGBA(image.Rect(0, 0, 120, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 120; x++ {
			flat.Set(x, y, color.RGBA{R: 20, G: 20, B: 20, A: 255})
		}
	}

	if _, err := ScrollOffset(flat, flat, region); !errors.Is(err, ErrOffsetUncertain) {
		t.Fatalf("got %v, want ErrOffsetUncertain", err)
	}
}

func TestScrollOffsetRejectsAnInvalidRegion(t *testing.T) {
	frame := stripedFrame(120, 800, 0)
	bad := transport.Rect{X1: 0.9, Y1: 0.2, X2: 0.1, Y2: 0.8}

	if _, err := ScrollOffset(frame, frame, bad); err == nil {
		t.Fatal("want an error for an inverted region, got nil")
	}
}
