package ingest

import "math"

// The weekly ranking is sorted descending by points, so once the assignment
// has fixed each row's rank, a row's value is bounded by its nearest
// confidently-parsed neighbours above and below.
//
// That is a structural check of the same species as roster ingest's "the
// group header states its own total": a check where the alternative is a
// tuned number. It is what makes two otherwise unsafe things safe -- accepting
// a low-confidence read, and retrying an empty one at PSM 13 -- because the
// failure both invite is a value manufactured out of neighbouring content, and
// a manufactured value has no reason to land inside a narrow ordered window.

// pointsBound is one row's inclusive range, derived from its neighbours.
type pointsBound struct {
	Lo, Hi int64
}

// pointsUnknown is the value a row carries in the caller's array when it has
// no confidently-parsed points -- used only for readability at call sites;
// pointsBounds itself never inspects values[i] where known[i] is false.
const pointsUnknown = int64(-1)

// pointsBounds returns one bound per row. values[i] is meaningful only where
// known[i]; unknown rows are bracketed by the nearest KNOWN neighbour on each
// side, looking past other unknowns rather than resting on a value that was
// never read. An end with no neighbour is left open (0 or math.MaxInt64)
// rather than pinned to something that does not exist.
//
// It trusts known[i] outright: it assumes the caller has already dropped any
// seed that would corrupt the ordering (see monotonicKnown below). It does
// not re-check monotonicity itself, because a bound function that silently
// discarded some of its own seeds would make pointsBounds' output depend on
// input it never reports discarding -- the filtering has to be visible at
// its own call site, not buried in here.
func pointsBounds(values []int64, known []bool) []pointsBound {
	out := make([]pointsBound, len(values))

	// Hi comes from the nearest known row ABOVE (a better rank scores at
	// least as much); Lo from the nearest known row below.
	hi := int64(math.MaxInt64)
	for i := range values {
		out[i].Hi = hi
		if known[i] {
			hi = values[i]
		}
	}
	lo := int64(0)
	for i := len(values) - 1; i >= 0; i-- {
		out[i].Lo = lo
		if known[i] {
			lo = values[i]
		}
	}
	return out
}

// withinBounds reports whether v satisfies b.
func withinBounds(v int64, b pointsBound) bool {
	return v >= b.Lo && v <= b.Hi
}

// monotonicKnown filters a seed set down to the ones consistent with the
// ranking's own non-increasing order, dropping (marking unknown) any seed
// that would break it.
//
// pointsBounds' bracketing argument assumes screen order equals rank order.
// That holds for a genuine ranking row, but a capture can also contain the
// pinned self row -- a second sighting of an already-ranked member at an
// arbitrary screen position, with its own points value. Task 3's assignment
// already keeps that row out of this array (assignments[n].Member == -1, so
// the caller never marks it known), but that guards only the pinned-row
// case. Nothing stops a genuine, confidently-read value that is simply out
// of order -- a mis-ranked row, or a duplicate the assignment did not
// catch -- from seeding a window too, and a bound resting on an out-of-order
// value is WORSE than no bound at all: it silently narrows its neighbours'
// windows to the wrong range and then rejects correct reads, rather than
// just failing to corroborate them.
//
// So seeds are walked in rank (screen) order and a seed greater than the
// last accepted one -- the sequence must be non-increasing -- is dropped
// rather than allowed to define a window. This costs nothing on a capture
// with no such row (the gate's capture has none, 86 deduped rows); it is a
// production-only safeguard against a capture that does.
func monotonicKnown(values []int64, known []bool) []bool {
	out := make([]bool, len(known))
	last := int64(math.MaxInt64)
	for i, v := range values {
		if !known[i] {
			continue
		}
		if v > last {
			continue // breaks the non-increasing sequence; not a valid seed
		}
		out[i] = true
		last = v
	}
	return out
}
