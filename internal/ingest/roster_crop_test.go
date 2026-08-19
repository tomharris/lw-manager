package ingest

import (
	"image"
	"image/png"
	"os"
	"sort"
	"testing"
)

// The member list's name column sits to the right of a per-member status icon,
// and this file pins the crop's left edge into the gutter between them.
//
// The defect it exists for: nameXFrac0 was 0.19 (x=137 of a 720px frame),
// which is inside that icon, so every member who has one had a fragment of it
// read as part of their name -- "7 Kun Tsunami", "} Lothar232", "P Ravenna
// Morrigan". 56 of 331 row bands on capture 1, against 53 that read a name
// exactly. It survived a review because the fractions were checked by cropping
// ten real rows and reading them back BY EYE, which is the one method
// CLAUDE.md says cannot work: a person reading a crop already knows what the
// name says, so "[icon fragment]Lothar232" reads as confirmation.
//
// The columns below were measured with `make probe-roster
// PROBE_ARGS=-roster.inkprofile`, an ink histogram over 68 row bands, and
// reproduced independently before being written down. Ink per column, name
// line only:
//
//	x=136 (0.1889)  787   <- the old nameXFrac0, mid-icon
//	x=155 (0.2153)  430       icon's right edge
//	x=156 (0.2167)   16   <- gutter
//	x=157 (0.2181)   16       gutter
//	x=158 (0.2194)   16       gutter
//	x=160 (0.2222)  946       the name's own first column
//
// The gutter is three columns wide, which is why this test checks three and
// why nameXFrac0 sits at its left end rather than its middle -- a member whose
// name begins one pixel early should lose nothing.
const (
	// statusIconX0/X1 bound the icon itself, and are used only by the guard
	// that keeps this test from passing vacuously on a fixture that has no
	// icon in it.
	statusIconX0, statusIconX1 = 124, 156

	// nameCropLeadColumns is how many columns right of the crop's left edge
	// must look like gutter. Three, the measured width of the gutter.
	nameCropLeadColumns = 3

	// maxGutterInk is the ink a genuine gutter may carry across those columns,
	// as a count of pixels over the whole band height. The measured gutter
	// carries 16 across 68 bands -- well under one pixel per band per column
	// -- so anything in single digits here is antialiasing and anything in the
	// hundreds is a UI element the crop should not be able to see.
	maxGutterInk = 8

	// rosterInkThreshold is summed |dR|+|dG|+|dB| against the scanline's own
	// median. Deviation rather than darkness: this list is orange text and
	// white-outlined text on cream, so a dark-pixel count -- the measure the VS
	// crops were placed with, against a dark ranking screen -- reads near zero
	// on every column here and would place every edge wrong.
	rosterInkThreshold = 90
)

func TestNameCropStartsInTheGutterRightOfTheStatusIcon(t *testing.T) {
	img := loadRosterBandFixture(t)
	b := img.Bounds()
	band := RowBand{Y0: b.Min.Y, Y1: b.Max.Y}
	rect := fieldRect(band, img, nameXFrac0, nameXFrac1, topRowYFrac0, topRowYFrac1)

	// The name line only, matching what the crop actually hands the engine.
	y0 := b.Min.Y + int(topRowYFrac0*float64(band.Height()))
	y1 := b.Min.Y + int(topRowYFrac1*float64(band.Height()))

	// Guard first. Without it this test would pass on any fixture whose row
	// happens to have no status icon, which is most of them -- and it would
	// then go on passing after a regression put the crop back inside the icon.
	// A test that cannot fail is worse than no test, and this is the shape
	// CLAUDE.md's "broken instrument reports agreement" warning takes here.
	if got := rosterBandInk(img, statusIconX0, statusIconX1, y0, y1); got <= maxGutterInk {
		t.Fatalf("fixture guard: expected the status icon to carry ink in x=%d..%d, got %d;"+
			" this fixture no longer exercises the defect and the assertion below proves nothing",
			statusIconX0, statusIconX1, got)
	}

	x0 := int(rect.X1 * float64(b.Dx()))
	got := rosterBandInk(img, x0, x0+nameCropLeadColumns, y0, y1)
	if got > maxGutterInk {
		t.Errorf("name crop starts at x=%d (nameXFrac0=%.4f) carrying %d ink in its first %d columns, want <= %d:"+
			" the crop's left edge is inside the status icon, not in the gutter beside it."+
			" Re-measure with `make probe-roster PROBE_ARGS=-roster.inkprofile` before moving it.",
			x0, nameXFrac0, got, nameCropLeadColumns, maxGutterInk)
	}
}

// rosterBandInk counts pixels in the half-open column range [x0,x1) and row
// range [y0,y1) that deviate from their own scanline's median colour.
func rosterBandInk(img image.Image, x0, x1, y0, y1 int) int {
	n := 0
	for y := y0; y < y1; y++ {
		med := rosterScanlineMedian(img, y)
		for x := x0; x < x1; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			d := rosterAbsDiff(r>>8, med[0]) + rosterAbsDiff(g>>8, med[1]) + rosterAbsDiff(b>>8, med[2])
			if int(d) > rosterInkThreshold {
				n++
			}
		}
	}
	return n
}

func rosterScanlineMedian(img image.Image, y int) [3]uint32 {
	b := img.Bounds()
	rs := make([]uint32, 0, b.Dx())
	gs := make([]uint32, 0, b.Dx())
	bs := make([]uint32, 0, b.Dx())
	for x := b.Min.X; x < b.Max.X; x++ {
		r, g, bl, _ := img.At(x, y).RGBA()
		rs = append(rs, r>>8)
		gs = append(gs, g>>8)
		bs = append(bs, bl>>8)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
	sort.Slice(gs, func(i, j int) bool { return gs[i] < gs[j] })
	sort.Slice(bs, func(i, j int) bool { return bs[i] < bs[j] })
	m := len(rs) / 2
	return [3]uint32{rs[m], gs[m], bs[m]}
}

func rosterAbsDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// loadRosterBandFixture returns one real row band of capture 1 -- full frame
// width, one memberRowPitch tall, carrying the member "Kain445" and the status
// icon beside the name.
func loadRosterBandFixture(t *testing.T) image.Image {
	t.Helper()

	f, err := os.Open("testdata/roster_row_status_icon.png")
	if err != nil {
		t.Fatalf("opening the row-band fixture: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decoding the row-band fixture: %v", err)
	}
	if got := img.Bounds().Dy(); got != memberRowPitch {
		t.Fatalf("fixture is %dpx tall, want one row pitch (%d); the field Y fractions are"+
			" fractions OF a band, so a fixture of another height measures a different crop",
			got, memberRowPitch)
	}
	return img
}
