// Package ingest turns stored capture frames into facts. It never touches a
// device: everything here reads decoded images and writes rows, which is what
// keeps its tests device-free and lets a parser fix be replayed over every
// capture ever taken.
package ingest

import (
	"errors"
	"fmt"
	"image"
	"log/slog"
	"math"
	"sort"

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
// This is a coarse periodicity sanity check, not a list detector, and the
// comment used to overclaim it was the latter. Measured across the
// committed corpus at pitch 112 over memberListRegion, screens that are not
// a member list routinely clear a much higher floor than this one --
// alliance_duel up to 93, vs_ranking_weekly up to 53, even _none frames up
// to 76 -- because plenty of UI that is not a member list is still
// periodic-ish over a short window. What actually keeps SegmentRows from
// running on a mail screen or a radar screen is the screen_id gate the
// caller applies before ever reaching this function (roster.go, vs.go);
// this floor's only job is to refuse a region with no periodic structure at
// all, and to decide when it's worth searching for a different pitch (see
// bestFittingPitch).
//
// 35 is set from confirmed-correctly-aligned real frames only: nine
// verified-aligned member-list frames from capture 1 measured 83.4-98.5 at
// pitch 112, comfortably clear. It sits just above a genuine constraint
// this floor cannot dodge by being lower: SegmentRows' own
// ErrPitchMismatch fixture (a region truly periodic at 160px, checked
// against an expected 112px) measures 29.56 at the wrong pitch -- a region
// that is NOT what the caller expects can still show meaningful contrast by
// partial accident, and this floor is what decides whether to trust it or
// go looking for a different pitch, so it must clear that number with
// margin. A genuinely sparse group -- a real frame with the list region
// otherwise blank below it (measured by artificially truncating a real
// frame's content) -- shows contrast 91.2/78.0/65.0/51.7/38.7/25.5 for
// 6/5/4/3/2/1 real rows, 12.7 with none: 2 or more sparse rows still clear
// 35, a lone final row does not and is silently treated as no list here --
// an accepted, pre-existing limitation (the original 30 didn't recover it
// either), not something this round changed.
//
// This constant was 30 before this round, justified by an observed range of
// "45-92" on real frames. The 45 was screenshot 80 of capture 1 -- one of
// the phase-misaligned frames collectBands now exists to fix (see its doc
// comment) -- so that range was partly measuring the defect, not real
// variation, and the floor was calibrated on its own failure case.
// Misalignment is no longer this constant's problem to catch;
// collectBands' per-boundary gap-brightness check is what refuses a
// misaligned phase now -- but that does not make this floor free to drop
// far, because the ErrPitchMismatch constraint above was never about
// misalignment in the first place.
const phaseContrastFloor = 35.0

// gapBrightnessMargin bounds how far a candidate row boundary's own sample
// may sit below the region's brightest scanline and still be trusted as
// landing in a genuine inter-card gap, rather than at the compromise offset
// a single global phase produces when two different real phases coexist in
// one region (see collectBands). Measured directly on the eight capture-1
// frames known to have this defect: a genuine gap sample sits within a
// couple of levels of the region's brightest scanline (245.5-248, in
// practice pinned to the true max); the compromise-phase samples on those
// same eight frames measured 195.2-211.0 -- a 34+ point gap in every case.
// 15 sits centered in that gap with margin on both sides.
const gapBrightnessMargin = 15.0

// maxRunRecursionDepth bounds how many times collectBands will re-attempt
// phase-locking on a leftover slice after extracting one confirmed run.
// Every one of the eight affected real frames measured had exactly one
// interposed element (one rank-group header dividing two groups' rows), so
// depth 1 -- try the leftover before the confirmed run and the leftover
// after it, each once, with no further recursion inside those attempts --
// recovered every row on every one of them. A frame with two or more
// interposed headers in a single capture has not been observed and would
// only be partially recovered at this depth; raise it if one ever is.
const maxRunRecursionDepth = 1

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
// A single global phase is not always enough, though: 8 of 61 real
// member-list frames in capture 1 have a rank-group header interposed
// partway down the region -- not a member row, and not one pitch tall --
// so the rows above it and the rows below it sit at two genuinely different
// phases. A phase locked across the whole region lands on whatever
// compromise offset best fits BOTH populations at once, which is not the
// true phase for either, and the resulting bands cut through the middle of
// real cards on both sides of the header with no error raised -- exactly
// the silent misalignment ErrPitchMismatch exists to prevent, just not the
// case it was built to catch. collectBands is the fix: it only trusts a
// boundary whose own sample sits close to the region's brightest scanline
// (gapBrightnessMargin), extracts the longest run of such boundaries, and
// re-locks phase on whatever is left before and after that run, recovering
// the second group's rows at their own, different phase rather than
// silently emitting the compromise.
//
// pitch is trusted as a known layout constant (112px roster, 128px VS
// ranking, per internal/tasks measurements) rather than searched for cold.
// If the region does not show strong enough periodicity at that pitch, a
// wider search either names a pitch that DOES fit -- evidence of a real
// layout change, reported as ErrPitchMismatch -- or finds nothing, meaning
// the region simply isn't showing a list right now (mid-scroll, empty
// state, wrong screen). See phaseContrastFloor's own comment for what that
// check does and does not guarantee.
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

	_, contrast, _ := phaseLock(profile, pitch)
	if contrast < phaseContrastFloor {
		// The region does not look periodic at the pitch we were told to
		// expect. Before giving up, check whether it fits a DIFFERENT pitch
		// well: that would mean the layout moved, which must fail loudly
		// rather than silently mis-segment. If nothing fits either, this
		// simply isn't a list right now -- which is indistinguishable, from
		// contrast alone, from a real list too diluted by blank space to
		// measure (see phaseContrastFloor's comment on the dilution curve).
		// Logging the contrast is what makes those two cases tell apart
		// later, since the return value alone cannot.
		if altPitch, altContrast := bestFittingPitch(profile, pitch); altContrast >= phaseContrastFloor {
			return nil, fmt.Errorf("ingest: region fits a %dpx pitch, not the expected %dpx: %w", altPitch, pitch, ErrPitchMismatch)
		}
		slog.Debug("ingest: no periodic row list found in region", "pitch", pitch, "contrast", contrast, "floor", phaseContrastFloor)
		return nil, nil
	}

	profileMax := sliceMax(profile)
	relBands := collectBands(profile, pitch, profileMax, maxRunRecursionDepth)
	if len(relBands) == 0 {
		// Contrast cleared the floor -- there is periodicity in here
		// somewhere -- but no boundary could be confirmed gap-bright enough
		// to trust (see collectBands). Distinct from the branch above, and
		// worth its own line for the same reason.
		slog.Debug("ingest: region is periodic but no boundary was confirmed as a genuine gap", "pitch", pitch, "contrast", contrast)
		return nil, nil
	}

	bands := make([]RowBand, len(relBands))
	for i, rb := range relBands {
		bands[i] = RowBand{Y0: top + rb.Y0, Y1: top + rb.Y1}
	}
	sort.Slice(bands, func(i, j int) bool { return bands[i].Y0 < bands[j].Y0 })
	return bands, nil
}

// collectBands finds every run of full-pitch bands within profile whose
// boundaries are confirmed as landing in a genuine gap, recovering a second
// group's rows at their own phase when an interposed element -- a
// rank-group header, whose height is not the row pitch -- splits the region
// into two differently-phased halves. profileMax is the brightest scanline
// in the ORIGINAL, top-level region (not recomputed per recursive slice):
// both screens render the gap at one fixed, near-maximum brightness
// throughout a single frame, so the top-level max is the right yardstick
// for every slice cut from it, including ones too short or too sparse to
// reliably measure their own.
//
// Offsets in the returned bands are relative to the start of profile, not
// the caller's absolute frame coordinates; SegmentRows translates them.
func collectBands(profile []float64, pitch int, profileMax float64, depth int) []RowBand {
	phase, contrast, n := phaseLock(profile, pitch)
	if contrast < phaseContrastFloor || n < 2 {
		return nil
	}

	type boundary struct {
		y         int
		confirmed bool
	}
	boundaries := make([]boundary, 0, n)
	for k := 0; k < n; k++ {
		y := phase + k*pitch
		if y >= len(profile) {
			break
		}
		boundaries = append(boundaries, boundary{y, profile[y] >= profileMax-gapBrightnessMargin})
	}

	// The longest run of consecutive confirmed boundaries is what this
	// phase actually got right; everything outside it is either the
	// interposed element itself or rows at a different phase.
	bestStart, bestLen := -1, 0
	for i := 0; i < len(boundaries); {
		if !boundaries[i].confirmed {
			i++
			continue
		}
		j := i
		for j < len(boundaries) && boundaries[j].confirmed {
			j++
		}
		if j-i > bestLen {
			bestStart, bestLen = i, j-i
		}
		i = j
	}
	if bestLen < 2 {
		// Not even two consecutive boundaries could be trusted -- nothing
		// safe to emit from this slice at this phase.
		return nil
	}

	bands := make([]RowBand, 0, bestLen-1)
	for k := bestStart; k < bestStart+bestLen-1; k++ {
		bands = append(bands, RowBand{Y0: boundaries[k].y, Y1: boundaries[k+1].y})
	}

	if depth > 0 {
		before := boundaries[bestStart].y
		if before >= pitch {
			bands = append(bands, collectBands(profile[:before], pitch, profileMax, depth-1)...)
		}
		after := boundaries[bestStart+bestLen-1].y
		if len(profile)-after >= pitch {
			for _, rb := range collectBands(profile[after:], pitch, profileMax, depth-1) {
				bands = append(bands, RowBand{Y0: after + rb.Y0, Y1: after + rb.Y1})
			}
		}
	}
	return bands
}

// sliceMax returns the largest value in xs. Callers only ever pass a
// non-empty profile (SegmentRows returns before this point on a short
// region), so an empty slice is not a case this needs to handle.
func sliceMax(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
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
// Contrast -- not the winning phase's absolute brightness -- is what tells
// SegmentRows whether to bother looking for bands at all. It is not, on its
// own, proof that every boundary at the winning phase is trustworthy: see
// collectBands, which checks each boundary's own sample against the
// region's brightest scanline rather than relying on the aggregate alone.
// This is the same winner-versus-runner-up shape ScrollOffset's probe
// agreement and the rank-badge matcher both landed on independently
// elsewhere in this milestone (see CLAUDE.md): real variation moves
// absolute brightness levels around from frame to frame but leaves the
// separation between the true phase and the rest roughly intact, so
// separation -- not an absolute level -- is the thing worth measuring.
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
// ErrPitchMismatch message. It does not need to be -- and on a single real
// frame with only six or seven periods in view, is not reliably -- the
// literal global winner over every neighbour of the CORRECT pitch: two
// candidate pitches one pixel apart can differ by less than sampling noise
// (measured directly on one real roster frame: pitch 112 and 113 scored
// contrast 45.4 and 46.9 respectively, a 3% gap on six samples each --
// that frame was later found to be one of the phase-misaligned ones
// collectBands now recovers, but the adjacency point holds regardless of
// alignment: a one-pixel difference in period length samples
// near-identical offsets and has no reason not to tie). SegmentRows
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
