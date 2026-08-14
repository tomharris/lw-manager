// Package ingest turns stored capture frames into facts. It never touches a
// device: everything here reads decoded images and writes rows, which is what
// keeps its tests device-free and lets a parser fix be replayed over every
// capture ever taken.
package ingest

import (
	"errors"
	"fmt"
	"image"
	"math"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrPitchMismatch reports that the region does not show the expected pitch
// as a periodic signal. It is a layout-change alarm: silently misaligned
// rows would keep producing plausible numbers from the wrong pixels.
var ErrPitchMismatch = errors.New("ingest: detected rows do not match the expected pitch")

// phaseContrastFloor is the minimum gap, in grayscale levels [0,255],
// between a candidate phase's mean brightness and the region's dimmest
// phase, below which a region is treated as not periodic at all.
//
// Measured on real frames (see
// docs/superpowers/specs/evidence/m4-ocr-2026-08-14, findings 6/6a, and the
// four roster frames from capture 1 / run 366 checked while implementing
// this): contrast ranged 45-92 on the member list and measured 92 on a real
// VS ranking frame. 30 sits comfortably below the weakest of those while
// staying well clear of the near-zero contrast a genuinely flat or
// non-periodic region produces -- the same shape as the required gap
// CLAUDE.md documents for NCC template matching: a threshold set against an
// absolute level is fragile, but a floor set well below every observed real
// separation is not.
const phaseContrastFloor = 30.0

// RowBand is one detected row, in pixel coordinates of the frame it came from.
type RowBand struct {
	Y0, Y1 int
}

// Height returns the band's height in pixels.
func (b RowBand) Height() int { return b.Y1 - b.Y0 }

// SegmentRows splits the list region into row bands by locating the PHASE of
// a known periodic pitch, rather than by thresholding brightness.
//
// The old approach assumed the row-mean brightness signal was bimodal:
// "cards are darker than the page background, so below the midpoint is card
// and above is gap." Real frames are not bimodal -- every row mixes light
// card background with dark text, which average together, so the row-mean
// signal oscillates through a wide band with no gap between two
// populations. There is no threshold that separates populations that do not
// exist (see docs/superpowers/specs/evidence/m4-ocr-2026-08-14, finding 6,
// for the real measurement that established this).
//
// What IS reliable is the phase: both list screens draw rows at a fixed,
// known pitch, and the inter-card gap is consistently the brightest thing
// on either screen. Sampling the row-mean profile at a candidate phase p,
// p+pitch, p+2*pitch, ... and averaging, the correct phase is the one whose
// samples land in those gaps -- and that holds even though the absolute
// brightness levels shift from frame to frame, because what is being
// measured is separation between phases, not an absolute level (finding
// 6a; see also phaseLock's doc comment).
//
// pitch is trusted as a known layout constant (112px roster, 128px VS
// ranking, per internal/tasks measurements) rather than searched for cold.
// If the region does not show strong enough periodicity at that pitch, a
// wider search either names a pitch that DOES fit -- evidence of a real
// layout change, reported as ErrPitchMismatch -- or finds nothing, meaning
// the region simply isn't showing a list right now (mid-scroll, empty
// state, wrong screen).
func SegmentRows(img image.Image, region transport.Rect, pitch int) ([]RowBand, error) {
	if !region.Valid() {
		return nil, fmt.Errorf("ingest: list region %+v is not a valid unit-square rect", region)
	}
	if pitch <= 0 {
		return nil, fmt.Errorf("ingest: pitch must be positive, got %d", pitch)
	}

	g := vision.Grayscale(img)
	b := g.Bounds()
	top := b.Min.Y + int(region.Y1*float64(b.Dy()))
	bot := b.Min.Y + int(region.Y2*float64(b.Dy()))
	x0 := b.Min.X + int(region.X1*float64(b.Dx()))
	x1 := b.Min.X + int(region.X2*float64(b.Dx()))
	if bot-top < pitch/2 || x1 <= x0 {
		return nil, nil
	}

	profile := rowMeans(g, top, bot, x0, x1)

	phase, contrast, _ := phaseLock(profile, pitch)
	if contrast < phaseContrastFloor {
		// The region does not look periodic at the pitch we were told to
		// expect. Before giving up, check whether it fits a DIFFERENT pitch
		// well: that would mean the layout moved, which must fail loudly
		// rather than silently mis-segment. If nothing fits either, this
		// simply isn't a list right now.
		if altPitch, altContrast := bestFittingPitch(profile, pitch); altContrast >= phaseContrastFloor {
			return nil, fmt.Errorf("ingest: region fits a %dpx pitch, not the expected %dpx: %w", altPitch, pitch, ErrPitchMismatch)
		}
		return nil, nil
	}

	// Boundaries sit exactly `pitch` apart starting at the locked phase.
	// Any leftover before the first boundary or after the last one is
	// shorter than a full pitch -- clipped by the region edge -- and is
	// dropped by construction rather than emitted and filtered after the
	// fact: a "band" here is only ever built at the true pitch, so there is
	// nothing left to measure once it exists. This replaces the old code's
	// after-the-fact sliver filter, which compared the median band
	// *height* (the card alone) against `pitch` (card plus gap) -- two
	// different quantities that differ by the gap, biasing the check even
	// on a healthy frame.
	var bands []RowBand
	for y := top + phase; y+pitch <= bot; y += pitch {
		bands = append(bands, RowBand{Y0: y, Y1: y + pitch})
	}
	return bands, nil
}

// rowMeans returns the mean grayscale brightness of each scanline in
// [top,bot) across columns [x0,x1).
func rowMeans(g *image.Gray, top, bot, x0, x1 int) []float64 {
	means := make([]float64, 0, bot-top)
	for y := top; y < bot; y++ {
		var sum float64
		for x := x0; x < x1; x++ {
			sum += float64(g.GrayAt(x, y).Y)
		}
		means = append(means, sum/float64(x1-x0))
	}
	return means
}

// phaseLock scores every candidate phase in [0,pitch) by the mean of
// profile[phase], profile[phase+pitch], profile[phase+2*pitch], ... and
// returns the best-scoring phase, together with contrast: the gap between
// that phase's mean and the worst-scoring phase's mean.
//
// Contrast -- not the winning phase's absolute brightness -- is what a
// caller should trust. This is the same winner-versus-runner-up shape
// ScrollOffset's probe agreement and the rank-badge matcher both landed on
// independently elsewhere in this milestone (see CLAUDE.md): real
// variation moves absolute brightness levels around from frame to frame
// but leaves the separation between the true phase and the rest roughly
// intact, so separation -- not an absolute level -- is the thing worth
// measuring.
func phaseLock(profile []float64, pitch int) (phase int, contrast float64, sampleCount int) {
	if pitch <= 0 || len(profile) == 0 {
		return 0, 0, 0
	}
	best, worst := math.Inf(-1), math.Inf(1)
	for p := 0; p < pitch; p++ {
		var sum float64
		var n int
		for y := p; y < len(profile); y += pitch {
			sum += profile[y]
			n++
		}
		if n == 0 {
			continue
		}
		mean := sum / float64(n)
		if mean > best {
			best, phase, sampleCount = mean, p, n
		}
		if mean < worst {
			worst = mean
		}
	}
	if math.IsInf(worst, 1) {
		return 0, 0, 0
	}
	return phase, best - worst, sampleCount
}

// bestFittingPitch searches candidate pitches from three-quarters of the
// expected value to one-and-a-half times it -- a real layout change moves
// the pitch by tens of percent (the roster/VS pitches themselves differ by
// 14%, and the fixture this is tested against moves 112 to 160, +43%), not
// by a multiple -- for the one whose phase-locked contrast, normalized for
// sample count, is highest.
//
// The normalization matters: a larger pitch samples fewer scanlines per
// phase (fewer full periods fit in a fixed-height region), so its raw
// best-vs-worst contrast carries more sampling variance and can win a naive
// comparison by luck alone rather than by genuinely fitting the region
// better. Scaling by sqrt(n) is the standard correction for comparing an
// extremal statistic across different sample sizes.
//
// This only ever runs once the expected pitch has already failed its own
// floor check in SegmentRows, purely to name a candidate for the
// ErrPitchMismatch message. It does not need to be -- and on a single noisy
// real frame with only six or seven periods in view, is not reliably -- the
// literal global winner over every neighbour of the CORRECT pitch: two
// candidate pitches one pixel apart can differ by less than sampling noise
// (measured directly: pitch 112 and 113 scored 45.4 and 46.9 respectively
// on one real roster frame, a 3% gap on six samples each). SegmentRows
// therefore accepts the expected pitch outright whenever its own contrast
// clears phaseContrastFloor, and only asks this function to search when it
// does not -- i.e. only when the expected pitch has already demonstrably
// stopped fitting, not as a per-frame popularity contest against its
// neighbours.
func bestFittingPitch(profile []float64, expected int) (pitch int, contrast float64) {
	lo := expected * 3 / 4
	if lo < 1 {
		lo = 1
	}
	hi := expected * 3 / 2

	bestNorm := -1.0
	for p := lo; p <= hi; p++ {
		_, c, n := phaseLock(profile, p)
		if n < 2 {
			continue
		}
		norm := c * math.Sqrt(float64(n))
		if norm > bestNorm {
			bestNorm, pitch, contrast = norm, p, c
		}
	}
	return pitch, contrast
}
