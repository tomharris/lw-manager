package ingest

import (
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// loadTestdataPNG decodes a fixture from internal/ingest/testdata. Fatal on
// any error: a missing or corrupt fixture is a test-setup bug, not a case
// under test.
func loadTestdataPNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening testdata %s: %v", path, err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decoding testdata %s: %v", path, err)
	}
	return img
}

// TestMatchRankBadgePicksTheRightRank is task 24's test 1: NCC picks the
// right rank for each of R1-R4 against real badge crops, with the measured
// gap asserted.
//
// The fixtures (testdata/rankbadge_r{1,2,3,4}.png) are real pixels, not
// synthetic shapes: each is evidence frame 06 (docs/superpowers/specs/
// evidence/m4-ocr-2026-08-14/06-all-four-rank-groups-collapsed.png, a real
// handset capture) with that rank's own badge-and-header band moved to sit
// at rankBadgeRegion's expected sticky-header position — this task's report
// documents the exact composition. Everything else in the frame (list
// rows, other UI chrome) is untouched real pixels.
//
// This is a same-frame measurement, the shape Finding 4a's own "worst
// cross-rank NCC" table used (docs/superpowers/specs/evidence/
// m4-ocr-2026-08-14 README, Finding 4a) — it is deliberately not the
// cross-capture measurement that section calls "the only test that means
// anything." Cross-capture requires a second capture's real frames, which
// live in the gitignored blob store (data/blobs/, see CLAUDE.md) rather
// than in git, so they cannot back a fixture-based test that has to pass
// with nothing running (invariant #6). That measurement was done by hand
// against capture 1's real frames (run 366) and is reported, not committed
// here — see this task's own report for the numbers (R4 gap 0.589, R3 gap
// 0.250, R2 gap 0.264, all cross-capture; R1 has no cross-capture probe at
// all, the same limitation rankBadgeMinGap's own doc comment carries).
func TestMatchRankBadgePicksTheRightRank(t *testing.T) {
	cases := []struct {
		rank   string
		file   string
		minGap float64 // lower bound, not the exact measured value: a same-source match is a self-consistency check, and the real (cross-capture) gaps measured by hand are smaller than what this same-frame setup produces.
	}{
		{"R1", "testdata/rankbadge_r1.png", 0.30},
		{"R2", "testdata/rankbadge_r2.png", 0.25},
		{"R3", "testdata/rankbadge_r3.png", 0.25},
		{"R4", "testdata/rankbadge_r4.png", 0.30},
	}
	for _, c := range cases {
		t.Run(c.rank, func(t *testing.T) {
			img := loadTestdataPNG(t, c.file)
			got, err := matchRankBadgeReal(img)
			if err != nil {
				t.Fatalf("matchRankBadgeReal: %v", err)
			}
			if got.Rank != c.rank {
				t.Errorf("Rank = %q, want %q (score %.3f, gap %.3f)", got.Rank, c.rank, got.Score, got.Gap)
			}
			if got.Gap < c.minGap {
				t.Errorf("Gap = %.3f, want at least %.3f (measured lower bound)", got.Gap, c.minGap)
			}
			t.Logf("%s: score=%.3f gap=%.3f", c.rank, got.Score, got.Gap)
		})
	}
}

// TestMatchRankBadgeRefusesAnUnconvincingMatch is task 24's test 2: a badge
// that matches nothing well enough must go to review rather than guessing a
// rank. The probe is a flat, featureless frame — nothing resembling a badge
// anywhere in it, the same "photographed the wrong thing" condition
// CLAUDE.md documents for a near-degenerate anchor crop, just deliberately
// pushed all the way to zero variance rather than merely "nearly" flat.
// matcher.go's ncc returns exactly 0 for a zero-variance search patch (the
// pVar==0 case), so every template scores identically and the gap between
// best and runner-up collapses to 0 — which must refuse, not pick an
// arbitrary "winner" among four ties.
//
// The brief calls this test out as the one that matters most and asks for
// it to be verified against its own regression: this is that verification.
// bestTwoRankScores (rankbadge.go) is matchRankBadgeReal's scoring step with
// the gap check not yet applied — calling it directly on the same blank
// frame proves two things at once: the failure above is caused by the gap
// comparison specifically (not an incidental Match error, e.g. the region
// admitting no placement), and that comparison is actually load-bearing
// (removing it would produce a confident-looking wrong verdict instead of
// an error).
func TestMatchRankBadgeRefusesAnUnconvincingMatch(t *testing.T) {
	blank := uniformImage(720, 1600, color.RGBA{R: 210, G: 210, B: 210, A: 255})

	if _, err := matchRankBadgeReal(blank); !errors.Is(err, ErrNoConfidentRank) {
		t.Fatalf("matchRankBadgeReal(blank) = %v, want ErrNoConfidentRank", err)
	}

	best, runnerUp, err := bestTwoRankScores(blank)
	if err != nil {
		t.Fatalf("bestTwoRankScores(blank): %v (want a winner and runner-up score to exist even though neither is confident — matching itself must succeed on a blank frame, only the acceptance rule should refuse it)", err)
	}
	gap := best.score - runnerUp.score
	if gap >= rankBadgeMinGap {
		t.Fatalf("gap %.3f already clears rankBadgeMinGap on a blank frame -- this test's premise (a badge that matches nothing well) does not hold", gap)
	}
	// This is the regression check itself: bestTwoRankScores just proved it
	// would happily name best.rank as "the" rank with no error at all if
	// nothing compared it to runnerUp — meaning the refusal above came from
	// the gap comparison in matchRankBadgeReal, and deleting that comparison
	// (the change this test exists to catch) would make
	// TestMatchRankBadgeRefusesAnUnconvincingMatch's first assertion fail:
	// matchRankBadgeReal would return best.rank instead of ErrNoConfidentRank.
	t.Logf("without the gap check, matchRankBadgeReal would have reported rank=%q at gap=%.3f instead of refusing", best.rank, gap)
}

// TestRankBadgeMinGapSeparatesBothDistributions pins rankBadgeMinGap from
// both sides. The review that prompted it established that nothing did: with
// the gap check itself intact, setting the constant to 0.001 left the entire
// internal/ingest suite passing, because every existing assertion is either a
// true match (gap >= 0.308) or the blank frame (gap exactly 0.000). Any value
// in (0, 0.30] was a silent weakening.
//
// The lower bound is a measurement, not a preference: shifting rankBadgeRegion
// 60px up off the sticky header makes R4 win at 0.717 over R1's 0.555 — gap
// 0.162, the worst of the whole near-miss sweep recorded in rankBadgeMinGap's
// doc comment, and the wrong rank. That read must be refused, so the constant
// has to sit above it, and the sub-test below asserts the refusal behaviourally
// rather than trusting the arithmetic.
//
// The upper bound is the smallest true gap ever measured, R3's cross-capture
// 0.250. A floor at or above it would send correct reads to review.
func TestRankBadgeMinGapSeparatesBothDistributions(t *testing.T) {
	const (
		worstNearMissGap = 0.162 // dy=-60, R4 0.717 over R1 0.555 -- wrong rank
		smallestTrueGap  = 0.250 // R3, cross-capture, capture 1's real headers
	)
	if rankBadgeMinGap <= worstNearMissGap {
		t.Errorf("rankBadgeMinGap = %.3f, must exceed the worst measured near-miss gap %.3f or it accepts a known wrong-rank read", rankBadgeMinGap, worstNearMissGap)
	}
	if rankBadgeMinGap >= smallestTrueGap {
		t.Errorf("rankBadgeMinGap = %.3f, must stay below the smallest true gap %.3f or it sends correct reads to review", rankBadgeMinGap, smallestTrueGap)
	}

	// The bounds above are arithmetic on remembered numbers; this re-runs the
	// probe that produced the lower one, so the test still means something if
	// a template, a fixture or the matcher changes underneath it.
	t.Run("the measured near-miss is actually refused", func(t *testing.T) {
		orig := rankBadgeRegion
		t.Cleanup(func() { rankBadgeRegion = orig })

		const dy = -60.0 / rankBadgeRefHeight
		rankBadgeRegion = transport.Rect{X1: orig.X1, Y1: orig.Y1 + dy, X2: orig.X2, Y2: orig.Y2 + dy}

		img := loadTestdataPNG(t, "testdata/rankbadge_r1.png")
		best, runnerUp, err := bestTwoRankScores(img)
		if err != nil {
			t.Fatalf("bestTwoRankScores on the shifted region: %v", err)
		}
		gap := best.score - runnerUp.score
		t.Logf("region shifted 60px up: best=%s %.3f runner-up=%s %.3f gap=%.3f", best.rank, best.score, runnerUp.rank, runnerUp.score, gap)

		if best.rank == "R1" {
			t.Skipf("premise gone: the shifted region still picks the fixture's true rank (gap %.3f), so this is no longer a near-miss to defend against", gap)
		}
		if _, err := matchRankBadgeReal(img); !errors.Is(err, ErrNoConfidentRank) {
			t.Errorf("matchRankBadgeReal on a region shifted off the header = %v, want ErrNoConfidentRank (it picked %s at gap %.3f)", err, best.rank, gap)
		}
	})
}

// uniformImage returns an image.Image every pixel of which is c — used for
// the "matches nothing" probe above, where the whole point is that no
// template can find any structure in it at all.
func uniformImage(w, h int, c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}
