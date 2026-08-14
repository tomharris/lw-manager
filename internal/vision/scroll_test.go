package vision

import (
	"errors"
	"image"
	"image/color"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// stripedFrame draws horizontal bands of varying grey so a vertical shift is
// unambiguous. A flat image would correlate with itself at every offset, which
// is the degenerate case ScrollOffset must not be tested against.
//
// The band value alone is not enough, and this fixture used to stop there: v
// was `((y+shift)/7*37) % 251`, which — within any window not wide enough to
// hit the mod-251 wraparound, i.e. every window any of these tests actually
// search — is a plain arithmetic sequence in the row-band index. NCC
// normalizes out a template's own mean and scale, so a linear ramp's *shape*
// after mean-subtraction is identical at every window position sharing the
// same local slope: it is the flat-crop trap CLAUDE.md documents for
// near-zero-variance templates, just wearing a non-flat disguise. That was
// invisible while ScrollOffset only ever asked "is this the best-scoring
// placement", but it produces an exact tie (peak == best competing placement)
// once ScrollOffset also asks "is it the *only* good placement" via
// offsetMinMargin, which every test in this file now exercises. The
// per-column marker below is what breaks the tie: its horizontal position
// advances with the row band on a different modulus than the band value's
// own wraparound, so no two row bands within these frames' height look alike
// after mean-subtraction, and only the truly-aligned placement scores well.
func stripedFrame(w, h, shift int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	markerW := w/6 + 1
	for y := 0; y < h; y++ {
		// +shift moves content up: the band that was at y+shift is now at y.
		Y := y + shift
		band := uint8((((Y) / 7) * 37) % 200) // capped below 255 so the marker always stands out
		markerX := (Y / 7 * 31) % w
		for x := 0; x < w; x++ {
			v := band
			if x >= markerX && x < markerX+markerW {
				v = 255
			}
			img.Set(x, y, color.RGBA{R: v, G: v, B: uint8((x / 5 * 11) % 251), A: 255})
		}
	}
	return img
}

// listLatticeFrame renders a synthetic scrollable list: a periodic row band
// (so uniformly-pitched rows correlate with themselves once per pitch — the
// exact lattice-decoy trap recon found on the handset) plus a marker column
// whose position is a deterministic pseudo-random function of the row index,
// standing in for a member's name or avatar. A simple arithmetic function of
// rowIdx (rowIdx*k mod w) was tried first and rejected: for the large shifts
// these tests need, it aliased back into a short cycle and produced
// coincidental near-matches at shift values that had nothing to do with the
// row pitch, which is a property of that formula, not of real list content.
// math/rand seeded per row has no such short period. pitch is the row height
// in pixels, standing in for memberRowPitch/vsRowPitch.
//
// The marker's width and amplitude are load-bearing, and got this wrong on
// the first pass: a full-height, full-contrast marker (255 against a 0-180
// band, occupying a fifth of the row) makes an off-by-N-pitch placement
// score around 0.3-0.4 — nowhere near the 0.86 a real blind-probe decoy
// scored on the handset (see the package doc on ScrollOffset), because the
// marker dominates the correlation instead of merely perturbing it. That
// made every acceptance criterion this file exists to protect look
// load-bearing without actually being exercised: a decoy that weak is
// already rejected by score or agreement long before admissibility or the
// geometry check get a chance to matter. markerAmp/markerW below are a
// narrow (8px of 140), moderate (+60, additive and clamped rather than a
// hard 255 override) perturbation instead, verified against the real
// evidence frames' shape with the scrolldiag tool before being trusted: at
// the real VS geometry (h=1600, pitch=128, shift=665 — see
// docs/superpowers/specs/evidence/m4-scrolloffset-2026-08-13/) this
// generator's own blind probe 2 scores 0.93 on its decoy, comfortably in
// the same range recon measured on hardware, not a fixture-only 0.39.
func listLatticeFrame(w, h, pitch, shift int) *image.RGBA {
	const markerW = 8
	const markerAmp = 60
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		Y := y + shift
		within := Y % pitch
		rowIdx := Y / pitch
		band := (within * 180) / pitch // periodic vertical ramp: identical in every row
		markerX := rand.New(rand.NewSource(int64(rowIdx))).Intn(w)
		for x := 0; x < w; x++ {
			v := band
			if x >= markerX && x < markerX+markerW {
				v += markerAmp
				if v > 255 {
					v = 255
				}
			}
			img.Set(x, y, color.RGBA{R: uint8(v), G: uint8(v), B: uint8(v), A: 255})
		}
	}
	return img
}

// corruptPixels overwrites a frac fraction of the pixels inside rect (a
// pixel-space image.Rectangle, not a normalized transport.Rect) with
// independent random noise, seeded for reproducibility. It stands in for
// whatever degrades a real crop's correlation without moving its position —
// motion blur mid-swipe, a compression artifact, a status-bar element
// briefly overlapping the strip — so a test can depress probe 0's own score
// without disturbing where its argmax actually falls.
func corruptPixels(img *image.RGBA, rect image.Rectangle, frac int, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			if rng.Intn(100) < frac {
				v := uint8(rng.Intn(256))
				img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
			}
		}
	}
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

// latticeRegion, latticeW/H and latticePitch reproduce the real VS-ranking
// geometry exactly (docs/superpowers/specs/evidence/m4-scrolloffset-
// 2026-08-13/) rather than a scaled-down stand-in: region {0.185, 0.80} at
// h=1600 gives regionH=984, stripH=118, and probe limits
//
//	probe 0 (y=296, the region top): limit 866px
//	probe 1 (y=414):                 limit 748px
//	probe 2 (y=532):                 limit 630px
//
// matching the evidence file's table exactly. Using the real numbers means a
// test's shift value can be read directly off the evidence (665px, the
// measured true travel) rather than re-derived, and it is what surfaced a
// real bug independent of this fixture: float64(414)/float64(1600), rounded
// back to pixels the way Crop truncates rather than the way Match rounds,
// evaluates to 413, not 414 (confirmed directly: 414.0/1600.0*1600.0 ==
// 413.999999999999943 in float64). That is now fixed at its source
// (pxFrac in scroll.go), not worked around here.
var latticeRegion = transport.Rect{X1: 0.03, Y1: 0.185, X2: 0.97, Y2: 0.80}

const (
	latticeW     = 140
	latticeH     = 1600
	latticePitch = 128
)

// TestScrollOffsetIgnoresABlindProbe is brief item 1, and the reason this
// task exists: a probe whose own search window cannot reach the true offset
// must not be able to decide the answer, even though — unlike a template
// that fails to match at all — it does not error. It just returns the best
// placement its truncated window contains, and for a uniformly-pitched list
// that is a lattice decoy: a wrong answer with a real, sometimes-high score
// (0.93 on this fixture — see listLatticeFrame's doc comment on why that
// realism matters).
//
// shift=665 is the real VS ranking pair's own measured true offset. It sits
// inside probe 0's and probe 1's reach (866px, 748px) but past probe 2's
// (630px), exactly the "two probes sighted, one blind" split recon measured
// on hardware. This is also this task's admissibility test: probe 2's own
// argmax (a confident decoy, not a low-scoring non-answer) must be excluded
// from voting entirely rather than out-argued by a stronger score, and it
// was verified by deleting the admissibility check (`if pr.limit <
// candidate`) in a scratch build and re-running this test — see the task
// report for the exact failure it produced.
func TestScrollOffsetIgnoresABlindProbe(t *testing.T) {
	const shift = 665
	prev := listLatticeFrame(latticeW, latticeH, latticePitch, 0)
	cur := listLatticeFrame(latticeW, latticeH, latticePitch, shift)

	got, err := ScrollOffset(prev, cur, latticeRegion)
	if err != nil {
		t.Fatalf("ScrollOffset: %v", err)
	}
	if got != shift {
		// Failing here with got as some other multiple of latticePitch away
		// from shift is exactly the defect this test exists to catch: a
		// lattice decoy from a probe that could never have seen the truth.
		t.Fatalf("offset = %d, want %d (delta %d px = %.1f row pitches)", got, shift, got-shift, float64(got-shift)/float64(latticePitch))
	}
}

// TestScrollOffsetRefusesBeyondEveryProbesReach is brief item 2: when the
// true travel exceeds even probe 0's reach — the largest of any probe — no
// candidate in this frame pair is correct, and ScrollOffset must say so
// rather than hand back whatever partial match it found. shift=3200
// reproduces the deliberate over-swipe evidence pair's own travel (700px in
// 250ms measured ~3200px against an 866px limit) almost exactly, rather than
// a shift chosen only to clear the limit arithmetically. On real hardware
// that pair refused via probe disagreement (757/629/502, a 255px spread),
// not via the geometry message, because periodic list content always gives
// every probe *some* placement to report; this fixture reproduces that same
// shape (checked with -v; probe argmaxes disagree by more than
// offsetAgreeTol here too), which is why this test asserts only
// errors.Is(ErrOffsetUncertain) rather than a specific message — see
// TestScrollOffsetRefusesWhenCandidateHugsProbeZerosLimit below for the
// narrower band where the geometry message specifically fires.
func TestScrollOffsetRefusesBeyondEveryProbesReach(t *testing.T) {
	const shift = 3200
	prev := listLatticeFrame(latticeW, latticeH, latticePitch, 0)
	cur := listLatticeFrame(latticeW, latticeH, latticePitch, shift)

	_, err := ScrollOffset(prev, cur, latticeRegion)
	if !errors.Is(err, ErrOffsetUncertain) {
		t.Fatalf("got %v, want ErrOffsetUncertain", err)
	}
}

// TestScrollOffsetRefusesWhenCandidateHugsProbeZerosLimit isolates the
// geometry check specifically (candidate > limit(0)-stripH), which the brief
// calls "the load-bearing check" and which C1 found untested: on the
// original weak-marker fixture, shift=750 never actually reached this
// branch (probe 0 itself already reported a low-scoring decoy at d=230,
// refused on score alone). shift=800 here sits inside probe 0's own 866px
// limit — so probe 0 correctly reports candidate=800, not a decoy — but past
// limit(0)-stripH=748, which is this design's load-bearing headroom
// requirement.
//
// It is also worth recording an algebraic fact this fixture exposed:
// limit(0)-stripH equals limit(1) exactly, for any region/stripH (both
// reduce to regionH-2*stripH). So whenever the geometry check fires, probe 1
// (if it survived the flatness filter) is simultaneously inadmissible for
// the same candidate, and the "no other probe's window reaches the
// candidate" refusal a few lines below would independently produce
// ErrOffsetUncertain too — just from a different code path, with a
// different message. errors.Is alone cannot distinguish "geometry caught
// this" from "agreement caught this once geometry didn't", so this test
// checks the message text specifically. Deleting the geometry check in a
// scratch build and re-running this test confirmed exactly that: the call
// still returned ErrOffsetUncertain (via "no other probe's window reaches
// candidate offset 800px"), so a bare errors.Is assertion would not have
// caught the check's removal — see the task report for the exact output.
func TestScrollOffsetRefusesWhenCandidateHugsProbeZerosLimit(t *testing.T) {
	const shift = 800
	prev := listLatticeFrame(latticeW, latticeH, latticePitch, 0)
	cur := listLatticeFrame(latticeW, latticeH, latticePitch, shift)

	_, err := ScrollOffset(prev, cur, latticeRegion)
	if !errors.Is(err, ErrOffsetUncertain) {
		t.Fatalf("got %v, want ErrOffsetUncertain", err)
	}
	if !strings.Contains(err.Error(), "reach limit") {
		t.Fatalf("error %q does not name probe 0's own reach limit — this must be the geometry check, not a different refusal reaching the same sentinel", err.Error())
	}
}

// TestScrollOffsetRefusesALowScoringPlacement isolates the score floor: a
// candidate that is geometrically valid and agrees with every admissible
// probe, but whose own placement is a poor match, must still be refused.
// corruptPixels stands in for whatever degrades a real crop without moving
// it — motion blur, a compression artifact, a momentary overlay — by
// overwriting 20% of probe 0's own source pixels in cur with independent
// noise; probe 1 and probe 2's own strips are untouched and still find and
// agree on the true position, so this isolates the score check from
// admissibility and agreement rather than conflating them. 20% depresses
// probe 0's score to ~0.73, comfortably below offsetMinScore (0.75) without
// approaching offsetMinVariance's flatness floor (noise raises variance, it
// does not lower it). Deleting the score check in a scratch build and
// re-running this test confirmed it then returns the correct offset instead
// of refusing — see the task report for the exact output.
func TestScrollOffsetRefusesALowScoringPlacement(t *testing.T) {
	const shift = 400
	prev := listLatticeFrame(latticeW, latticeH, latticePitch, 0)
	cur := listLatticeFrame(latticeW, latticeH, latticePitch, shift)

	regionTop := int(math.Round(latticeRegion.Y1 * float64(latticeH)))
	regionBot := int(math.Round(latticeRegion.Y2 * float64(latticeH)))
	stripH := int(offsetStripFrac * float64(regionBot-regionTop))
	probe0Rect := image.Rect(0, regionTop, latticeW, regionTop+stripH)
	corruptPixels(cur, probe0Rect, 20, 7)

	_, err := ScrollOffset(prev, cur, latticeRegion)
	if !errors.Is(err, ErrOffsetUncertain) {
		t.Fatalf("got %v, want ErrOffsetUncertain", err)
	}
	if !strings.Contains(err.Error(), "scored") {
		t.Fatalf("error %q does not name a score — this must be the score check, not a different refusal reaching the same sentinel", err.Error())
	}
}

// TestScrollOffsetRefusesWhenProbesDisagree is brief item 3, and distinct
// from the reach test above: here the true offset (400px) is well inside
// every probe's own limit, so an honest probe 1 would agree with probe 0.
// This plants an exact copy of probe 1's own strip (as cut from cur) at the
// very start of probe 1's search window in prev — a perfect, if planted,
// decoy at d=0. Match's placement loop scans a probe's window from its own
// row downward and keeps the first placement to reach a given score (see
// matcher.go: `score > best.Score`, not `>=`), so a tied score at d=0 beats
// the genuine tie at d=400 purely by being encountered first. Probe 1 then
// reports d=0, which does not corroborate probe 0's (correct) candidate of
// 400 — and because probe 2 still agrees at 400, this now specifically
// exercises the "EVERY admissible probe must agree" rule this fix round
// added: under the old "any admissible probe agrees" scheme this same setup
// would have passed, since probe 2's agreement alone used to be enough.
// Deleting the "every" requirement (reverting to "any") in a scratch build
// and re-running this test confirmed it: the call then succeeds and returns
// 400 instead of refusing — see the task report for the exact output.
func TestScrollOffsetRefusesWhenProbesDisagree(t *testing.T) {
	const shift = 400
	prev := listLatticeFrame(latticeW, latticeH, latticePitch, 0)
	cur := listLatticeFrame(latticeW, latticeH, latticePitch, shift)

	regionTop := int(math.Round(latticeRegion.Y1 * float64(latticeH)))
	regionBot := int(math.Round(latticeRegion.Y2 * float64(latticeH)))
	stripH := int(offsetStripFrac * float64(regionBot-regionTop))
	probe1Y := regionTop + 1*stripH

	for y := probe1Y; y < probe1Y+stripH; y++ {
		for x := 0; x < latticeW; x++ {
			prev.Set(x, y, cur.At(x, y))
		}
	}

	_, err := ScrollOffset(prev, cur, latticeRegion)
	if !errors.Is(err, ErrOffsetUncertain) {
		t.Fatalf("got %v, want ErrOffsetUncertain", err)
	}
}

// TestScrollOffsetRejectsAFlatStripByPerPixelVariance is brief item 4: this
// strip's summed variance (see matcher.go's variance(), which sums squared
// deviations rather than averaging them) clears the old, unreachable
// sum-based floor of 50 by three orders of magnitude, so the pre-fix
// bestProbeStrip would have accepted it as a probe. Per pixel — the unit
// offsetMinVariance is defined in now — it is 4.0, far below the 100 floor:
// this is the "checkbox is unchecked" trap from CLAUDE.md's NCC section
// wearing a different amplitude, and per-pixel variance is what a flat
// crop above the old floor still needs to be caught by.
func TestScrollOffsetRejectsAFlatStripByPerPixelVariance(t *testing.T) {
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8}
	w, h := 120, 800
	nearFlat := func() *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				v := uint8(100 + (x+y)%7) // sum variance ~46,000; per-pixel ~4
				img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
			}
		}
		return img
	}
	frame := nearFlat()

	_, err := ScrollOffset(frame, frame, region)
	if !errors.Is(err, ErrOffsetUncertain) {
		t.Fatalf("got %v, want ErrOffsetUncertain", err)
	}
}

// TestScrollOffsetMeasuresZeroOnARealList is brief item 5: the zero case
// must survive the rewrite, exercised here against listLatticeFrame rather
// than only stripedFrame, so a still real-shaped list (not just the basic
// fixture) is covered too.
func TestScrollOffsetMeasuresZeroOnARealList(t *testing.T) {
	frame := listLatticeFrame(latticeW, latticeH, latticePitch, 0)

	got, err := ScrollOffset(frame, frame, latticeRegion)
	if err != nil {
		t.Fatalf("ScrollOffset: %v", err)
	}
	if got != 0 {
		t.Fatalf("offset = %d, want 0", got)
	}
}
