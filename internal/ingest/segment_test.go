package ingest

import (
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// cardFrame draws `n` dark cards of height `card` separated by light gaps of
// height `gap`, starting at y=top. This is the shape of both list screens:
// distinct cards on a lighter page background.
func cardFrame(w, h, top, card, gap, n int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	light := color.RGBA{R: 250, G: 240, B: 240, A: 255}
	dark := color.RGBA{R: 60, G: 70, B: 110, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, light)
		}
	}
	y := top
	for i := 0; i < n; i++ {
		for yy := y; yy < y+card && yy < h; yy++ {
			for x := 0; x < w; x++ {
				img.Set(x, yy, dark)
			}
		}
		y += card + gap
	}
	return img
}

func TestSegmentRowsFindsEveryCard(t *testing.T) {
	// 6 cards of 100px with 12px gaps, starting at y=200. Pitch is 112.
	img := cardFrame(200, 1000, 200, 100, 12, 6)
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9}

	bands, err := SegmentRows(img, region, 112)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	if len(bands) != 6 {
		t.Fatalf("got %d bands, want 6: %+v", len(bands), bands)
	}
	if bands[0].Y0 != 200 {
		t.Errorf("first band starts at %d, want 200", bands[0].Y0)
	}
	for i, b := range bands {
		if got := b.Y1 - b.Y0; got != 100 {
			t.Errorf("band %d height = %d, want 100", i, got)
		}
	}
}

func TestSegmentRowsClipsToTheRegion(t *testing.T) {
	// Cards run the full frame, but the region excludes the top 300px, which
	// is how the sticky group header and the pinned self row are dropped.
	img := cardFrame(200, 1000, 0, 100, 12, 9)
	region := transport.Rect{X1: 0, Y1: 0.3, X2: 1, Y2: 1.0}

	bands, err := SegmentRows(img, region, 112)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	for _, b := range bands {
		if b.Y0 < 300 {
			t.Fatalf("band %+v starts above the region floor of 300", b)
		}
	}
}

func TestSegmentRowsRejectsAPitchThatDoesNotMatch(t *testing.T) {
	img := cardFrame(200, 1000, 200, 100, 12, 6)
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9}

	// The real pitch is 112. A layout change that moved it to 160 must be
	// loud, because silently misaligned rows produce plausible wrong numbers.
	if _, err := SegmentRows(img, region, 160); !errors.Is(err, ErrPitchMismatch) {
		t.Fatalf("got %v, want ErrPitchMismatch", err)
	}
}

func TestSegmentRowsOnAnEmptyRegionReturnsNoBands(t *testing.T) {
	img := cardFrame(200, 1000, 0, 100, 12, 0)
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9}

	bands, err := SegmentRows(img, region, 112)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	if len(bands) != 0 {
		t.Fatalf("got %d bands, want 0", len(bands))
	}
}
