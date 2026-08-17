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
// algorithm (git history) before this test was made to pass: it failed with
// "ingest: rows measure 2 px against an expected pitch of 112" -- a false
// ErrPitchMismatch from a median band height of 2px against a real pitch of
// 112, the same failure shape (tiny slivers, not clean rows) finding 6
// measured on the real corpus (30 bands, median height 3px there), from
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

// interposedHeaderFrame draws two periodic runs separated by a band that is
// NOT one pitch tall and is not gap-bright either -- a rank-group header
// inline in the scrolling list, the shape round-2 review found on 8 of 61
// real capture-1 member-list frames (70, 75, 80, 86, 91, 96, 101, 106). The
// header's height not being a multiple of pitch is the point: it shifts the
// second run's true phase relative to the first run's, so a single global
// phase cannot fit both.
func interposedHeaderFrame(w, h, top, pitch, gapHeight, headerHeight, firstRuns, secondRuns int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	headerEnd := top + firstRuns*pitch + headerHeight
	secondEnd := headerEnd + secondRuns*pitch
	for y := 0; y < h; y++ {
		v := uint8(200)
		switch {
		case y >= top && y < top+firstRuns*pitch:
			if off := (y - top) % pitch; off >= pitch-gapHeight {
				v = 246
			} else {
				v = 90
			}
		case y >= top+firstRuns*pitch && y < headerEnd:
			v = 150 // the header itself: distinct, and deliberately not gap-bright
		case y >= headerEnd && y < secondEnd:
			if off := (y - headerEnd) % pitch; off >= pitch-gapHeight {
				v = 246
			} else {
				v = 90
			}
		}
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: v})
		}
	}
	return img
}

// TestSegmentRowsRecoversBothSidesOfAnInterposedHeader is the regression
// test for the round-2 CRITICAL finding: a single global phase-lock across
// the whole region cannot fit a header-interposed list, because the rows
// above and below the header sit at two different phases and the global
// argmax lands on a compromise offset that matches neither -- previously
// emitted as ordinary-looking bands with no error, cutting through the
// middle of real cards on both sides.
//
// Confirmed against the round-1 algorithm (single global phase, no
// per-boundary confirmation) before collectBands replaced it: it returned 5
// bands -- {300,412} {412,524} {524,636} {636,748} {748,860} -- treating the
// whole region as one contiguous run. The header sits at [536,576), so the
// third and fourth bands both straddle it, each holding the tail of one
// real card and the head of the next: exactly the mid-card-cut corruption
// round-2 review found on real frames, reproduced here without needing the
// blob store. Verified directly against the real frames themselves too
// while building the fix (screenshots 70, 75, 80, 86, 91, 96, 101, 106 of
// capture 1): every one now segments into only full-pitch bands with none
// crossing the header, matching every one of the 9 confirmed-aligned frames
// checked alongside them.
func TestSegmentRowsRecoversBothSidesOfAnInterposedHeader(t *testing.T) {
	const top, pitch, gap, headerHeight = 200, 112, 12, 40
	img := interposedHeaderFrame(200, 1000, top, pitch, gap, headerHeight, 3, 3)
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.912} // y in [200,912)

	bands, err := SegmentRows(img, region, pitch)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	if len(bands) != 4 {
		t.Fatalf("got %d bands, want 4 (2 recovered from each run either side of the header): %+v", len(bands), bands)
	}

	headerStart, headerEnd := top+3*pitch, top+3*pitch+headerHeight
	for i, b := range bands {
		if got := b.Height(); got != pitch {
			t.Errorf("band %d height = %d, want %d -- a compromise-phase band leaked through", i, got, pitch)
		}
		if b.Y0 < headerEnd && b.Y1 > headerStart {
			t.Errorf("band %d = %+v overlaps the header [%d,%d) -- exactly the mid-card cut this test guards against", i, b, headerStart, headerEnd)
		}
	}
	// Both sides recovered, not just the one the global phase happened to
	// match: at least one band entirely before the header and at least one
	// entirely after it.
	var haveBefore, haveAfter bool
	for _, b := range bands {
		if b.Y1 <= headerStart {
			haveBefore = true
		}
		if b.Y0 >= headerEnd {
			haveAfter = true
		}
	}
	if !haveBefore {
		t.Error("no band recovered before the header -- one side was dropped, not just the compromise avoided")
	}
	if !haveAfter {
		t.Error("no band recovered after the header -- one side was dropped, not just the compromise avoided")
	}
}

// There used to be a TestSegmentRowsOnANonPeriodicRegionFindsNothing here,
// built around a hand-picked "irregular bands" fixture (nonListFrame) tuned
// to sit under phaseContrastFloor. It was dropped rather than kept, because
// round-2 review measured what it was implicitly claiming -- that a
// non-periodic, non-list region reliably fails this floor -- against the
// real committed corpus, and that claim is false: at pitch 112 over
// memberListRegion, alliance_duel frames measure up to 93, vs_ranking_weekly
// up to 53, even _none frames up to 76, fully overlapping real member-list
// frames' own 83-98. A hand-picked synthetic fixture that sits under the
// floor proves that fixture sits under the floor, not that the floor
// rejects non-list content in general -- the same selection bias the
// fixture's own doc comment already warned about for two earlier, more
// broken attempts, just not spotted in the one that shipped. See
// phaseContrastFloor's doc comment: the floor is a coarse periodicity
// check, and the real guarantee that SegmentRows never runs on a mail or
// radar screen is the screen_id gate in roster.go/vs.go, upstream of this
// package. What a synthetic fixture in this file CAN still honestly prove
// -- and what TestSegmentRowsOnAnEmptyRegionReturnsNoBands below covers --
// is that a truly flat, zero-variation region produces no bands.

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
