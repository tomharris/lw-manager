package ingest

import (
	"context"
	"math"
	"testing"
)

func TestPointsBoundsBracketAnUnknownRowByItsNeighbours(t *testing.T) {
	// Row 1 is unknown; rows 0 and 2 parsed. Descending order puts row 1
	// between them, inclusive.
	values := []int64{9_000_000, 0, 7_000_000}
	known := []bool{true, false, true}
	got := pointsBounds(values, known)
	if got[1].Lo != 7_000_000 || got[1].Hi != 9_000_000 {
		t.Errorf("row 1 bounds = %+v, want Lo 7000000 Hi 9000000", got[1])
	}
}

func TestPointsBoundsAreOpenAtTheEnds(t *testing.T) {
	// Nothing above rank 1 and nothing below the last row, so those sides are
	// unconstrained rather than pinned to a neighbour that does not exist.
	values := []int64{0, 5_000_000, 0}
	known := []bool{false, true, false}
	got := pointsBounds(values, known)
	if got[0].Lo != 5_000_000 || got[0].Hi != math.MaxInt64 {
		t.Errorf("row 0 bounds = %+v, want Lo 5000000 Hi MaxInt64", got[0])
	}
	if got[2].Lo != 0 || got[2].Hi != 5_000_000 {
		t.Errorf("row 2 bounds = %+v, want Lo 0 Hi 5000000", got[2])
	}
}

func TestPointsBoundsSkipConsecutiveUnknownRows(t *testing.T) {
	// Two unknowns in a row must both look past each other to the nearest
	// KNOWN neighbour, or the bound would rest on a value that was never read.
	values := []int64{9_000_000, 0, 0, 6_000_000}
	known := []bool{true, false, false, true}
	got := pointsBounds(values, known)
	for _, i := range []int{1, 2} {
		if got[i].Lo != 6_000_000 || got[i].Hi != 9_000_000 {
			t.Errorf("row %d bounds = %+v, want Lo 6000000 Hi 9000000", i, got[i])
		}
	}
}

func TestPointsBoundsOnAllUnknown(t *testing.T) {
	values := []int64{0, 0}
	known := []bool{false, false}
	got := pointsBounds(values, known)
	for i, b := range got {
		if b.Lo != 0 || b.Hi != math.MaxInt64 {
			t.Errorf("row %d bounds = %+v, want fully open", i, b)
		}
	}
}

// A row that reads higher than the row ranked above it cannot be a genuine
// ranking row -- the ranking is sorted descending -- so it must not be
// allowed to seed a bound. This is the case the brief's own examples never
// exercise: a value greater than the previous ACCEPTED seed, not merely out
// of numeric order with its immediate array neighbour.
func TestMonotonicKnownDropsASeedThatBreaksDescendingOrder(t *testing.T) {
	// Row 1 reads higher than row 0, which cannot happen in a genuine
	// ranking -- e.g. a pinned self row landing at an arbitrary screen
	// position, or a mis-ordered read the assignment did not catch.
	values := []int64{9_000_000, 12_000_000, 7_000_000}
	known := []bool{true, true, true}
	got := monotonicKnown(context.Background(), values, known)
	if !got[0] {
		t.Errorf("row 0 known = %v, want true (nothing before it to violate)", got[0])
	}
	if got[1] {
		t.Errorf("row 1 known = %v, want false: 12,000,000 > row 0's 9,000,000 breaks descending order", got[1])
	}
	if !got[2] {
		t.Errorf("row 2 known = %v, want true: 7,000,000 is consistent with row 0's accepted 9,000,000", got[2])
	}
}

// The out-of-order seed must not corrupt its NEIGHBOURS' windows either --
// this is the failure the controller ruling exists to prevent. Feeding
// monotonicKnown's output into pointsBounds should bracket row 1 (now
// unknown) using rows 0 and 2, not accept row 1's bogus 12,000,000 as a
// legitimate ceiling for row 2.
func TestMonotonicKnownFeedsCorrectWindowsIntoPointsBounds(t *testing.T) {
	values := []int64{9_000_000, 12_000_000, 7_000_000}
	known := monotonicKnown(context.Background(), values, []bool{true, true, true})
	got := pointsBounds(values, known)
	if got[1].Lo != 7_000_000 || got[1].Hi != 9_000_000 {
		t.Errorf("row 1 bounds = %+v, want Lo 7000000 Hi 9000000 (bracketed by rows 0 and 2, not seeded by itself)", got[1])
	}
	if got[2].Lo != 0 || got[2].Hi != 9_000_000 {
		t.Errorf("row 2 bounds = %+v, want Lo 0 Hi 9000000 (row 1's dropped seed must not lower its ceiling)", got[2])
	}
}

// A long run of consistent seeds followed by one violation: the violation is
// dropped but does not poison seeds that come after it and are themselves
// consistent with the last GOOD seed.
func TestMonotonicKnownRecoversAfterADroppedViolation(t *testing.T) {
	values := []int64{10_000_000, 15_000_000, 8_000_000, 6_000_000}
	known := monotonicKnown(context.Background(), values, []bool{true, true, true, true})
	want := []bool{true, false, true, true}
	for i, w := range want {
		if known[i] != w {
			t.Errorf("row %d known = %v, want %v", i, known[i], w)
		}
	}
}

// Rank 1 carries the longest number on screen -- the row most exposed to a
// left-edge clip -- and a greedy walk from the front trusts it absolutely:
// one clipped-but-confident rank-1 read would drop every later, correct
// seed as "greater than" it, leaving zero constraints for the whole capture.
// monotonicKnown instead keeps the LARGEST mutually-consistent set, so a bad
// seed anywhere in the run costs only itself.
func TestMonotonicKnownRecoversFromABadLeadingSeed(t *testing.T) {
	// Row 0 is a left-clipped rank-1 read: confidently read, numerically
	// tiny. A greedy prefix walk accepts it and then rejects every
	// subsequent (correct, descending) seed as "greater than" it. The
	// longest-subsequence approach instead drops only row 0.
	values := []int64{1_234, 17_000_000, 16_000_000, 15_000_000}
	known := []bool{true, true, true, true}
	got := monotonicKnown(context.Background(), values, known)
	want := []bool{false, true, true, true}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d known = %v, want %v", i, got[i], w)
		}
	}
}

// Non-increasing PERMITS equality -- two members can legitimately score the
// same -- and nothing else pins that down: a refactor from >= to > in the DP
// comparison would silently drop every tied member's seed while the rest of
// the suite stayed green, since every other case here has strictly
// decreasing values.
func TestMonotonicKnownPermitsTies(t *testing.T) {
	cases := []struct {
		name   string
		values []int64
		known  []bool
		want   []bool
	}{
		{
			name:   "a tie at the front is fully kept",
			values: []int64{100, 100, 90},
			known:  []bool{true, true, true},
			want:   []bool{true, true, true},
		},
		{
			name:   "a tie mid-run is fully kept",
			values: []int64{200, 100, 100, 50},
			known:  []bool{true, true, true, true},
			want:   []bool{true, true, true, true},
		},
		{
			name:   "an all-tied run is fully kept",
			values: []int64{100, 100, 100},
			known:  []bool{true, true, true},
			want:   []bool{true, true, true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := monotonicKnown(context.Background(), tc.values, tc.known)
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("row %d known = %v, want %v", i, got[i], w)
				}
			}
		})
	}
}

func TestRepairPointsSolvesASingleSymbolWhenTheBoundsAdmitOneDigit(t *testing.T) {
	// "¢,609,299" with a window that admits only a leading 2.
	got, ok := repairPoints("¢,609,299", pointsBound{Lo: 2_600_000, Hi: 2_613_585})
	if !ok || got != 2_609_299 {
		t.Errorf("repairPoints = %d, %v; want 2609299, true", got, ok)
	}
}

func TestRepairPointsRefusesWhenTheBoundsAdmitTwoDigits(t *testing.T) {
	// A window wide enough for both 1,609,299 and 2,609,299 determines
	// nothing, and guessing is exactly what this must not do.
	if got, ok := repairPoints("¢,609,299", pointsBound{Lo: 1_000_000, Hi: 3_000_000}); ok {
		t.Errorf("repairPoints = %d, true; want refusal: the bounds admit more than one digit", got)
	}
}

func TestRepairPointsRefusesMoreThanOneBadCharacter(t *testing.T) {
	// Two unknowns is a guess with extra steps, whatever the bounds say.
	if got, ok := repairPoints("¢,6¢9,299", pointsBound{Lo: 2_600_000, Hi: 2_613_585}); ok {
		t.Errorf("repairPoints = %d, true; want refusal on two damaged positions", got)
	}
}

func TestRepairPointsRefusesWhenTheGroupingIsBroken(t *testing.T) {
	// Without intact comma grouping there is no shape to solve against, and
	// the string could be anything.
	//
	// Exactly ONE damaged position, deliberately. This used to read
	// "¢6092 99", which carries two (the ¢ and the space) and is therefore
	// refused by the test above's rule before grouping is ever consulted --
	// so it passed without exercising the property it is named for.
	if got, ok := repairPoints("¢6092299", pointsBound{Lo: 2_600_000, Hi: 2_613_585}); ok {
		t.Errorf("repairPoints = %d, true; want refusal on broken grouping", got)
	}
}

// The three properties the controller ruling requires of trimPointsArtifact,
// each verified against the exact string capture 6 produced or the exact
// failure mode it must not create.

func TestTrimPointsArtifactStripsATrailingSymbol(t *testing.T) {
	// Rank 20's actual read: every digit is right, and only a trailing space
	// plus em-dash stand between this and a clean parse.
	got := trimPointsArtifact("12,090,000 —")
	if got != "12,090,000" {
		t.Errorf("trimPointsArtifact = %q, want %q", got, "12,090,000")
	}
	v, err := ParsePoints(got)
	if err != nil || v != 12_090_000 {
		t.Errorf("ParsePoints(%q) = %d, %v; want 12090000, nil", got, v, err)
	}
}

func TestTrimPointsArtifactDoesNotRescueACropThatCaughtTheNextRow(t *testing.T) {
	// The failure this trim could mask: a crop wide enough to catch the next
	// row's points too. That string ends in a DIGIT, not a symbol, so a
	// trailing-only trim must leave it untouched -- and it must still fail
	// to parse, because writing this row's value into this row would be
	// wrong in a way no review queue entry could catch.
	got := trimPointsArtifact("12,090,000 8,671,806")
	if got != "12,090,000 8,671,806" {
		t.Errorf("trimPointsArtifact = %q, want it unchanged (trailing char is a digit)", got)
	}
	if _, err := ParsePoints(got); err == nil {
		t.Errorf("ParsePoints(%q) succeeded; want ErrUnparseable", got)
	}
}

func TestTrimPointsArtifactNeverTouchesALeadingCorruption(t *testing.T) {
	// Rank 77's actual read. Stripping the leading "¢" would produce a
	// shorter, differently-grouped number wrong by roughly a factor of ten,
	// silently. trimPointsArtifact must leave this exact string alone --
	// recovery here is repairPoints' job, fenced by the bounds, not this
	// function's.
	got := trimPointsArtifact("¢,609,299")
	if got != "¢,609,299" {
		t.Errorf("trimPointsArtifact = %q, want it unchanged (leading corruption must never be trimmed)", got)
	}
	if _, err := ParsePoints(got); err == nil {
		t.Errorf("ParsePoints(%q) succeeded; want ErrUnparseable", got)
	}
}
