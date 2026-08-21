package vision

import (
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// solid builds a uniformly coloured RGBA image, the simplest fixture that
// still exercises real pixel math.
func solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestGrayscaleMapsExtremes(t *testing.T) {
	white := Grayscale(solid(4, 4, color.RGBA{255, 255, 255, 255}))
	if got := white.GrayAt(0, 0).Y; got != 255 {
		t.Errorf("white → gray: got %d, want 255", got)
	}

	black := Grayscale(solid(4, 4, color.RGBA{0, 0, 0, 255}))
	if got := black.GrayAt(2, 2).Y; got != 0 {
		t.Errorf("black → gray: got %d, want 0", got)
	}
}

func TestGrayscalePreservesBounds(t *testing.T) {
	g := Grayscale(solid(7, 3, color.RGBA{10, 20, 30, 255}))
	if b := g.Bounds(); b.Dx() != 7 || b.Dy() != 3 {
		t.Errorf("bounds not preserved: got %dx%d, want 7x3", b.Dx(), b.Dy())
	}
}

// colGradient builds a w×h gray image whose every pixel equals its x column,
// so a crop can be checked by reading back the column value it exposes.
func colGradient(w, h int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g.SetGray(x, y, color.Gray{Y: uint8(x)})
		}
	}
	return g
}

// narrowBand builds a 10×h gray image whose columns take values base..base+9,
// i.e. a deliberately low-contrast image for the equalizer to stretch.
func narrowBand(h int, base uint8) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, 10, h))
	for y := 0; y < h; y++ {
		for x := 0; x < 10; x++ {
			g.SetGray(x, y, color.Gray{Y: base + uint8(x)})
		}
	}
	return g
}

func TestEqualizeStretchesToFullRange(t *testing.T) {
	eq := Equalize(narrowBand(4, 100)) // values 100..109

	lo := eq.GrayAt(0, 0).Y
	hi := eq.GrayAt(9, 0).Y
	if lo != 0 {
		t.Errorf("darkest column after equalize: got %d, want 0", lo)
	}
	if hi != 255 {
		t.Errorf("brightest column after equalize: got %d, want 255", hi)
	}
}

func TestAdaptiveThresholdIsolatesLocalOutliers(t *testing.T) {
	// A single dark pixel on a bright field: the classic case adaptive
	// thresholding exists for. A global threshold near 200 would erase it.
	img := image.NewGray(image.Rect(0, 0, 9, 9))
	for y := 0; y < 9; y++ {
		for x := 0; x < 9; x++ {
			img.SetGray(x, y, color.Gray{Y: 200})
		}
	}
	img.SetGray(4, 4, color.Gray{Y: 0})

	bin := AdaptiveThreshold(img, 3, 2)
	if got := bin.GrayAt(4, 4).Y; got != 0 {
		t.Errorf("dark outlier: got %d, want 0", got)
	}
	if got := bin.GrayAt(0, 0).Y; got != 255 {
		t.Errorf("uniform-bright corner: got %d, want 255", got)
	}
}

func TestInvertSwapsBlackAndWhite(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 2, 1))
	img.SetGray(0, 0, color.Gray{Y: 0})
	img.SetGray(1, 0, color.Gray{Y: 255})

	inv := Invert(img)
	if inv.GrayAt(0, 0).Y != 255 || inv.GrayAt(1, 0).Y != 0 {
		t.Errorf("invert: got (%d,%d), want (255,0)", inv.GrayAt(0, 0).Y, inv.GrayAt(1, 0).Y)
	}
}

func TestUpscaleNearestNeighbour(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 2, 2))
	img.SetGray(0, 0, color.Gray{Y: 10})
	img.SetGray(1, 0, color.Gray{Y: 20})

	up := Upscale(img, 3)
	if b := up.Bounds(); b.Dx() != 6 || b.Dy() != 6 {
		t.Fatalf("upscaled size: got %dx%d, want 6x6", b.Dx(), b.Dy())
	}
	// Every pixel of the top-left source cell maps to value 10.
	g := color.GrayModel.Convert(up.At(2, 2)).(color.Gray).Y
	if g != 10 {
		t.Errorf("upscaled cell value: got %d, want 10", g)
	}
}

func TestCropTakesNormalizedRegion(t *testing.T) {
	// Right half of a 10-wide image: columns 5..9.
	c := Crop(colGradient(10, 8), transport.Rect{X1: 0.5, Y1: 0, X2: 1.0, Y2: 1.0})

	if b := c.Bounds(); b.Dx() != 5 || b.Dy() != 8 {
		t.Fatalf("cropped size: got %dx%d, want 5x8", b.Dx(), b.Dy())
	}
	// The crop's own origin (0,0) is the source's column 5.
	got := color.GrayModel.Convert(c.At(c.Bounds().Min.X, c.Bounds().Min.Y)).(color.Gray).Y
	if got != 5 {
		t.Errorf("crop origin column: got %d, want 5", got)
	}
}

// GreenChannel must return the green channel and nothing else. The obvious
// wrong implementation returns luma anyway (or the red channel, which the
// probe measured as the worst of the three on the field this exists for), and
// on a photograph of real UI both would look plausible.
func TestGreenChannelReturnsGreenNotLuma(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, color.RGBA{R: 200, G: 10, B: 30, A: 255})
	src.Set(1, 0, color.RGBA{R: 10, G: 200, B: 30, A: 255})

	g := GreenChannel(src)
	for x, want := range []uint8{10, 200} {
		got := color.GrayModel.Convert(g.At(x, 0)).(color.Gray).Y
		if got != want {
			t.Errorf("GreenChannel at x=%d = %d, want %d", x, got, want)
		}
	}
	if g.Bounds() != src.Bounds() {
		t.Errorf("bounds = %v, want %v", g.Bounds(), src.Bounds())
	}
}

// The reason it is a separate image rather than a Options flag: it must
// compose with Preprocess, whose own Grayscale step has to become a no-op.
func TestGreenChannelSurvivesPreprocess(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			src.Set(x, y, color.RGBA{R: 200, G: uint8(20 * (x + 1)), B: 30, A: 255})
		}
	}
	out := Preprocess(GreenChannel(src), Options{
		SkipEqualize: true, SkipThreshold: true, SkipInvert: true, UpscaleFactor: 1,
	})
	for x, want := range []uint8{20, 40, 60, 80} {
		if got := out.GrayAt(x, 0).Y; got != want {
			t.Errorf("preprocessed green channel at x=%d = %d, want %d", x, got, want)
		}
	}
}
