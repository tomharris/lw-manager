package transport

import (
	"image"
	"math"
	"testing"
)

func TestNormValid(t *testing.T) {
	for _, tc := range []struct {
		p    Norm
		want bool
	}{
		{Norm{0, 0}, true},
		{Norm{1, 1}, true},
		{Norm{0.5, 0.5}, true},
		{Norm{-0.01, 0.5}, false},
		{Norm{0.5, 1.01}, false},
	} {
		if got := tc.p.Valid(); got != tc.want {
			t.Errorf("Norm%v.Valid() = %v, want %v", tc.p, got, tc.want)
		}
	}
}

// The whole point of normalized coordinates: the same Norm lands at the
// proportionally same place on any resolution, so a new device never needs
// the template library re-captured.
func TestNormPixelsScalesAcrossResolutions(t *testing.T) {
	p := Norm{X: 0.5, Y: 0.25}

	for _, size := range []image.Point{
		{X: 1080, Y: 2400},
		{X: 720, Y: 1600},
		{X: 1440, Y: 3200},
	} {
		x, y := p.Pixels(size)
		if x != size.X/2 {
			t.Errorf("at %v, x = %d, want %d", size, x, size.X/2)
		}
		if y != size.Y/4 {
			t.Errorf("at %v, y = %d, want %d", size, y, size.Y/4)
		}
	}
}

func TestRectCenterAndValid(t *testing.T) {
	r := Rect{X1: 0.2, Y1: 0.4, X2: 0.6, Y2: 0.8}
	if !r.Valid() {
		t.Fatal("Rect.Valid() = false for a well-formed rect")
	}
	c := r.Center()
	const eps = 1e-9
	if math.Abs(c.X-0.4) > eps || math.Abs(c.Y-0.6) > eps {
		t.Errorf("Center() = %v, want (0.4, 0.6)", c)
	}

	if (Rect{X1: 0.6, Y1: 0.1, X2: 0.2, Y2: 0.5}).Valid() {
		t.Error("Valid() = true for an inverted rect")
	}
	if (Rect{X1: 0, Y1: 0, X2: 1.5, Y2: 1}).Valid() {
		t.Error("Valid() = true for a rect outside the unit square")
	}
}

func TestEscapeInputText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"hello", "hello"},
		{"hello world", `hello%sworld`},
		{"a&b", `a\&b`},
		{"$(rm -rf)", `\$\(rm%s-rf\)`},
	} {
		if got := escapeInputText(tc.in); got != tc.want {
			t.Errorf("escapeInputText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
