package ingest

import (
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
	got := monotonicKnown(values, known)
	if got[0] != true {
		t.Errorf("row 0 known = %v, want true (nothing before it to violate)", got[0])
	}
	if got[1] != false {
		t.Errorf("row 1 known = %v, want false: 12,000,000 > row 0's 9,000,000 breaks descending order", got[1])
	}
	if got[2] != true {
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
	known := monotonicKnown(values, []bool{true, true, true})
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
	known := monotonicKnown(values, []bool{true, true, true, true})
	want := []bool{true, false, true, true}
	for i, w := range want {
		if known[i] != w {
			t.Errorf("row %d known = %v, want %v", i, known[i], w)
		}
	}
}
