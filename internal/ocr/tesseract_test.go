package ocr

import (
	"context"
	"image"
	"image/color"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// renderLine draws s as black text on a white background using a pure-Go
// bitmap font — no CGO, no external asset. It stands in for a captured game
// numeral so the tesseract path has something real to read.
func renderLine(s string) image.Image {
	w, h := len(s)*14+20, 30
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.Gray{Y: 0}),
		Face: basicfont.Face7x13,
		Dot:  fixed.P(10, 20),
	}
	d.DrawString(s)
	return img
}

func TestTesseractReadsDigits(t *testing.T) {
	eng := NewTesseractEngine()
	if !eng.Available() {
		t.Skip("tesseract not installed; skipping real-engine test (see CLAUDE.md)")
	}

	// Upscale so the bitmap font's small glyphs are legible to tesseract.
	src := renderLine("12345")
	spec := Spec{Charset: "0123456789", MinConf: 0.3}
	res, err := eng.Read(context.Background(), upscaleGray(src, 4), spec)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := spec.Clean(res.Text); !strings.Contains(got, "12345") {
		t.Errorf("read text: got %q, want to contain 12345 (conf %.2f)", got, res.Confidence)
	}
}

// upscaleGray is a tiny nearest-neighbour enlarge, local to the test to avoid
// importing the vision package (which would couple these two layers).
func upscaleGray(src image.Image, f int) image.Image {
	b := src.Bounds()
	out := image.NewGray(image.Rect(0, 0, b.Dx()*f, b.Dy()*f))
	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := 0; x < out.Bounds().Dx(); x++ {
			out.Set(x, y, src.At(b.Min.X+x/f, b.Min.Y+y/f))
		}
	}
	return out
}
