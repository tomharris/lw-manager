package ingest

import (
	"errors"
	"image"
	"image/color"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// cardFrame draws `n` dark cards of height `card` separated by light gaps of
// height `gap`, starting at y=top. This is a clean bimodal fixture -- real
// frames are not this tidy (see periodicFrame below and finding 6 in
// docs/superpowers/specs/evidence/m4-ocr-2026-08-14) -- but it is still a
// valid periodic list, and roster_test.go/vs_test.go use it for higher-level
// ingest fixtures where the pixel content only needs to be geometrically
// plausible, not realistic.
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

// periodicFrame draws a strictly periodic list region: every `pitch` rows,
// the last `gapHeight` rows are a bright, flat inter-card gap (matching the
// real screens, where the gap is reliably the brightest thing on either
// list), and the remaining `pitch-gapHeight` rows are filled by cardLevel,
// which controls how "card-like" -- i.e. how far from bimodal -- the body of
// each row is.
//
// Built directly as *image.Gray so the profile SegmentRows measures is
// exactly what this function specifies, with no RGB-to-gray rounding to
// account for.
func periodicFrame(w, h, top, pitch, gapHeight int, cardLevel func(offsetInCard int) uint8) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		var v uint8 = 200
		if y >= top {
			off := (y - top) % pitch
			if off >= pitch-gapHeight {
				v = 246
			} else {
				v = cardLevel(off)
			}
		}
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: v})
		}
	}
	return img
}

// flatCardLevel returns a card body of one constant shade: a clean bimodal
// fixture, easy for either algorithm. Used only to sanity-check that basic
// periodic segmentation works at all before testing the case that matters.
func flatCardLevel(shade uint8) func(int) uint8 {
	return func(int) uint8 { return shade }
}

// noisyCardLevel oscillates the card body between roughly shade-40 and
// shade+40 using a fixed, deterministic waveform (no math/rand, so the
// fixture is stable across Go versions). This is the shape CLAUDE.md and
// finding 6 describe on real frames: light card background and dark text
// averaging together per scanline, so the row-mean signal wanders through a
// wide band with no gap between two populations -- min/p10/median/p90 close
// together, nothing resembling two clusters either side of a midpoint.
func noisyCardLevel(shade uint8) func(int) uint8 {
	return func(off int) uint8 {
		delta := 40 * math.Sin(float64(off)*1.3)
		v := float64(shade) + delta
		if v < 0 {
			v = 0
		}
		if v > 255 {
			v = 255
		}
		return uint8(v)
	}
}

func TestSegmentRowsFindsEveryPeriodInASyntheticList(t *testing.T) {
	// 6 periods of a clean, flatly-shaded card at pitch 112 (100px card body,
	// 12px bright gap), the same pitch measured on the roster.
	const top, pitch, gap = 200, 112, 12
	img := periodicFrame(200, 1000, top, pitch, gap, flatCardLevel(90))
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9} // y in [200,900)

	bands, err := SegmentRows(img, region, pitch)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	// The region is 700px tall, but the locked phase need not fall exactly
	// on the region's top edge -- it locks onto the gap, wherever in the
	// first period that lands -- so only whole periods after that first
	// boundary count: 5 full 112px periods fit in the 600px remaining after
	// the phase-aligned start, not 6.
	if len(bands) != 5 {
		t.Fatalf("got %d bands, want 5: %+v", len(bands), bands)
	}
	for i, b := range bands {
		if got := b.Height(); got != pitch {
			t.Errorf("band %d height = %d, want %d", i, got, pitch)
		}
		if i > 0 && b.Y0 != bands[i-1].Y1 {
			t.Errorf("band %d starts at %d, want contiguous with previous band's end %d", i, b.Y0, bands[i-1].Y1)
		}
	}
}

// TestSegmentRowsHandlesNonBimodalRealisticSignal is the regression test for
// the actual bug: the old algorithm assumed row-mean brightness was bimodal
// ("cards are darker than the page background, so below the midpoint is
// card and above is gap") and split at the midpoint between the darkest and
// brightest scanline. Real frames have no such split -- see finding 6 in
// docs/superpowers/specs/evidence/m4-ocr-2026-08-14: measured scanline means
// of min=149 p10=179 median=200 p90=223 max=246, oscillating continuously
// with no gap between two populations, because every row mixes light card
// background with dark text.
//
// This fixture reproduces that shape rather than the old tests' clean
// dark-card-on-light-page split: the card body oscillates continuously
// through a wide band (noisyCardLevel), and only the inter-card gap is
// reliably, consistently brighter. Confirmed against the pre-replacement
// algorithm (git history) before this test was made to pass: it produced 34
// bands over 6 real periods, median height 3px against the 112px pitch --
// the same failure shape findng 6 measured on the real corpus, from
// hunting the noise for midpoint crossings instead of locking onto the
// period.
func TestSegmentRowsHandlesNonBimodalRealisticSignal(t *testing.T) {
	const top, pitch, gap = 200, 112, 12
	img := periodicFrame(200, 1000, top, pitch, gap, noisyCardLevel(200))
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9}

	bands, err := SegmentRows(img, region, pitch)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	if len(bands) != 5 {
		t.Fatalf("got %d bands, want 5: %+v", len(bands), bands)
	}
	for i, b := range bands {
		if got := b.Height(); got != pitch {
			t.Errorf("band %d height = %d, want %d", i, got, pitch)
		}
	}
}

// nonListFrame draws irregular-width bands of varying shade -- like a real
// non-list screen's header bars, buttons and text blocks, which vary in
// height and tone but not on any fixed pitch -- via a small seeded LCG, so
// band widths and shades don't fall into a pattern a human hand-picking
// "irregular" numbers tends to produce by accident.
//
// Two earlier attempts at this fixture were both accidentally periodic in
// ways that only showed up once measured, which is worth recording so the
// next person doesn't repeat them: two smooth, incommensurate sine waves
// summed together produce a beat pattern that is itself periodic over some
// longer span, and independent per-scanline random noise -- maximally
// "non-periodic" in the abstract -- turns out to have a high chance of a
// spuriously good phase purely from picking the best of ~pitch candidates
// out of only six or seven samples each (measured: several different hash
// constants all landed contrast in the high 30s to 50s, overlapping real
// list frames' own 45-92). A real non-list screen does not vary
// scanline-to-scanline like that; it is spatially coherent, like the bands
// this builds. Seed 2 was picked because it measures a comfortable 18.2
// across the *entire* pitch search range SegmentRows uses
// (bestFittingPitch's 84-168 window for a 112 expected pitch), not just at
// 112 itself -- the failure mode that sank the first hand-picked attempt,
// where a neighbouring pitch (not the expected one) was what accidentally
// lined up.
func nonListFrame(w, h, top, bot int) *image.Gray {
	seed := 2
	state := uint32(seed*2654435761 + 12345)
	next := func(lo, hi int) int {
		state = state*1664525 + 1013904223
		return lo + int(state>>16)%(hi-lo)
	}
	type band struct {
		from, to int
		v        uint8
	}
	var bands []band
	for pos := 0; pos < bot-top; {
		width := next(25, 95)
		bands = append(bands, band{pos, pos + width, uint8(next(165, 245))})
		pos += width
	}

	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		v := uint8(200)
		if y >= top && y < bot {
			o := y - top
			for _, band := range bands {
				if o >= band.from && o < band.to {
					v = band.v
					break
				}
			}
		}
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: v})
		}
	}
	return img
}

func TestSegmentRowsOnANonPeriodicRegionFindsNothing(t *testing.T) {
	// A region with no consistent period at all: brightness wanders but
	// nothing repeats at any stable offset. No phase should stand out, so
	// there is nothing to hand downstream as a row.
	img := nonListFrame(200, 1000, 200, 900)
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9}

	bands, err := SegmentRows(img, region, 112)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	if len(bands) != 0 {
		t.Fatalf("got %d bands, want 0 (no period should stand out): %+v", len(bands), bands)
	}
}

func TestSegmentRowsRejectsAPitchThatDoesNotMatch(t *testing.T) {
	// The region is genuinely periodic, but at 160px -- a layout change from
	// the 112px the caller expects. That must fail loudly, naming both
	// values, rather than silently segmenting at the wrong pitch.
	const top, truePitch, gap = 200, 160, 16
	img := periodicFrame(200, 1200, top, truePitch, gap, flatCardLevel(90))
	region := transport.Rect{X1: 0, Y1: 0.15, X2: 1, Y2: 0.95}

	_, err := SegmentRows(img, region, 112)
	if !errors.Is(err, ErrPitchMismatch) {
		t.Fatalf("got %v, want ErrPitchMismatch", err)
	}
	if !strings.Contains(err.Error(), "112") {
		t.Errorf("error %q does not name the expected pitch (112)", err.Error())
	}
	// The named alternative need not land on the literal integer 160: on a
	// short window (six or seven periods) several pixel-adjacent candidate
	// pitches sample identically and tie exactly (measured directly: 157
	// through 163 all score the same on this fixture). Any of them is
	// correct evidence of a layout change; only a wildly different number,
	// or none at all, would indicate the search itself is broken.
	m := pitchInMsgRE.FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("error %q does not name a detected pitch", err.Error())
	}
	detected, _ := strconv.Atoi(m[1])
	if detected < truePitch-10 || detected > truePitch+10 {
		t.Errorf("named pitch %d is not within 10px of the true pitch %d", detected, truePitch)
	}
}

var pitchInMsgRE = regexp.MustCompile(`fits a (\d+)px pitch`)

func TestSegmentRowsDropsClippedEdgeBands(t *testing.T) {
	// Cards run the full frame, but the region excludes the top 300px (the
	// sticky group header and the pinned self row) and stops mid-period at
	// the bottom, so the first and last periods are clipped by the region
	// edge and must not appear in the output.
	const top, pitch, gap = 0, 112, 12
	img := periodicFrame(200, 1000, top, pitch, gap, flatCardLevel(90))
	region := transport.Rect{X1: 0, Y1: 0.3, X2: 1, Y2: 0.97} // y in [300,970)

	bands, err := SegmentRows(img, region, pitch)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	if len(bands) == 0 {
		t.Fatal("got no bands at all")
	}
	for _, b := range bands {
		if b.Y0 < 300 || b.Y1 > 970 {
			t.Fatalf("band %+v extends outside the region [300,970)", b)
		}
		if b.Height() != pitch {
			t.Fatalf("band %+v is not a full pitch (%d) tall -- a clipped edge band leaked through", b, pitch)
		}
	}
}

func TestSegmentRowsOnAnEmptyRegionReturnsNoBands(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 200, 1000))
	for y := 0; y < 1000; y++ {
		for x := 0; x < 200; x++ {
			img.SetGray(x, y, color.Gray{Y: 210})
		}
	}
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9}

	bands, err := SegmentRows(img, region, 112)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	if len(bands) != 0 {
		t.Fatalf("got %d bands, want 0", len(bands))
	}
}
