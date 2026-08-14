package vision

import (
	"errors"
	"image"
	"image/color"
	"math/rand"
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
func listLatticeFrame(w, h, pitch, shift int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	markerW := w / 7
	for y := 0; y < h; y++ {
		Y := y + shift
		within := Y % pitch
		rowIdx := Y / pitch
		band := uint8((within * 180) / pitch) // periodic vertical ramp: identical in every row
		markerX := rand.New(rand.NewSource(int64(rowIdx))).Intn(w)
		for x := 0; x < w; x++ {
			v := band
			if x >= markerX && x < markerX+markerW {
				v = 255
			}
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
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

// latticeRegion and latticePitch are shared by every test below that needs a
// probe to run out of reach at different depths. Region height 800px over
// offsetStripFrac=0.12 gives stripH=96, so the three probes' own search
// windows can prove at most:
//
//	probe 0 (y=100, the region top): limit 704px
//	probe 1 (y=196):                 limit 608px
//	probe 2 (y=292):                 limit 512px
//
// which is the same shape as the real VS-ranking measurement in
// docs/superpowers/specs/evidence/m4-scrolloffset-2026-08-13/ (limits
// 866/748/630 against a true travel of 665px) scaled down for a fast test.
var latticeRegion = transport.Rect{X1: 0, Y1: 0.1, X2: 1, Y2: 0.9}

const (
	latticeW     = 140
	latticeH     = 1000
	latticePitch = 40
)

// TestScrollOffsetIgnoresABlindProbe is brief item 1, and the reason this
// task exists: a probe whose own search window cannot reach the true offset
// must not be able to decide the answer, even though — unlike a template
// that fails to match at all — it does not error. It just returns the best
// placement its truncated window contains, and for a uniformly-pitched list
// that is a lattice decoy: a wrong answer with a real, sometimes-high score.
//
// shift=550 sits inside probe 0's and probe 1's reach (704px, 608px) but
// past probe 2's (512px), the same "some probes sighted, one blind"
// situation recon measured on the real VS ranking pair (probe 2's limit of
// 630 fell short of the true 665px travel there too). The old scheme picked
// its one probe by variance alone, with no notion of which probes could even
// see the answer; this asserts the new scheme's probe-0-sourced candidate
// is correct regardless of what a blind probe would have reported on its
// own — probe 2's wrong answer is discarded by the admissibility check
// before it ever gets a vote, not out-argued by a stronger score.
func TestScrollOffsetIgnoresABlindProbe(t *testing.T) {
	const shift = 550
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
// rather than hand back whatever partial match it found. shift=750 clears
// probe 0's own 704px limit.
func TestScrollOffsetRefusesBeyondEveryProbesReach(t *testing.T) {
	const shift = 750
	prev := listLatticeFrame(latticeW, latticeH, latticePitch, 0)
	cur := listLatticeFrame(latticeW, latticeH, latticePitch, shift)

	_, err := ScrollOffset(prev, cur, latticeRegion)
	if !errors.Is(err, ErrOffsetUncertain) {
		t.Fatalf("got %v, want ErrOffsetUncertain", err)
	}
}

// TestScrollOffsetRefusesWhenProbesDisagree is brief item 3, and distinct
// from the reach test above: here the true offset (300px) is well inside
// every probe's own limit, so an honest probe 1 would agree with probe 0.
// This plants an exact copy of probe 1's own strip (as cut from cur) at the
// very start of probe 1's search window in prev — a perfect, if planted,
// decoy at d=0. Match's placement loop scans a probe's window from its own
// row downward and keeps the first placement to reach a given score (see
// matcher.go: `score > best.Score`, not `>=`), so a tied score at d=0 beats
// the genuine tie at d=300 purely by being encountered first. Probe 1 then
// reports d=0, which does not corroborate probe 0's (correct) candidate of
// 300 — and because probe 2's own limit falls short of 300 here, probe 1 was
// the only probe that could have voted at all, so no admissible probe
// agrees and ScrollOffset must refuse rather than trust probe 0 alone.
func TestScrollOffsetRefusesWhenProbesDisagree(t *testing.T) {
	const shift = 550 // probe 2 inadmissible for this candidate; only probe 1 could corroborate
	prev := listLatticeFrame(latticeW, latticeH, latticePitch, 0)
	cur := listLatticeFrame(latticeW, latticeH, latticePitch, shift)

	regionTop := int(latticeRegion.Y1 * float64(latticeH))
	regionH := int(latticeRegion.Y2*float64(latticeH)) - regionTop
	stripH := int(offsetStripFrac * float64(regionH))
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
