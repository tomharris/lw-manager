package ingest

import (
	"context"
	"log/slog"
	"math"
)

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

// pointsBounds returns one bound per row. values[i] is meaningful only where
// known[i]; unknown rows are bracketed by the nearest KNOWN neighbour on each
// side, looking past other unknowns rather than resting on a value that was
// never read. An end with no neighbour is left open (0 or math.MaxInt64)
// rather than pinned to something that does not exist.
//
// values and known must be the same length and index the same row; both call
// sites build them together today, so this is not enforced, only documented.
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

// withinBounds reports whether v satisfies b. An open end (Lo == 0 or
// Hi == math.MaxInt64) always satisfies its side, which is exactly why
// clearing withinBounds is not by itself proof of corroboration -- see
// pointsOrderConfidenceFloor's comment in vs.go, which is what stops that
// gap from being exploited at the write gate.
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
// This is the LARGEST subset of seeds mutually consistent in rank order, not
// the prefix consistent with whichever seed came first. A greedy walk from
// the front trusts the first seed absolutely -- and rank 1 carries the
// longest number on the screen, the row most exposed to a left-edge clip on
// a crop that starts at x=0.74. A single clipped rank-1 value (read
// confidently, because a clip does not make a crop blurry) would otherwise
// be accepted as the seed and drop every later, correct seed as "greater
// than" it, leaving zero constraints for the entire capture. Finding the
// longest non-increasing subsequence instead means one bad seed anywhere in
// the run only costs itself, not everything after it.
//
// Non-increasing PERMITS equality: two members can legitimately score the
// same, and a run like [100, 100, 90] keeps every seed.
//
// O(n^2) on the number of seeds, which is bounded by the row count -- at
// most a couple hundred -- so the naive DP is not worth complicating.
func monotonicKnown(ctx context.Context, values []int64, known []bool) []bool {
	var idx []int // indices into values/known that are seeds, in rank order
	for i, k := range known {
		if k {
			idx = append(idx, i)
		}
	}
	out := make([]bool, len(known))
	if len(idx) == 0 {
		return out
	}

	// length[j] is the longest non-increasing run ending at seed j; prev[j]
	// is the seed before it in that run, or -1 to start one.
	length := make([]int, len(idx))
	prev := make([]int, len(idx))
	bestEnd, bestLen := 0, 0
	for j := range idx {
		length[j] = 1
		prev[j] = -1
		for k := 0; k < j; k++ {
			if values[idx[k]] >= values[idx[j]] && length[k]+1 > length[j] {
				length[j] = length[k] + 1
				prev[j] = k
			}
		}
		if length[j] > bestLen {
			bestLen, bestEnd = length[j], j
		}
	}

	for j := bestEnd; j != -1; j = prev[j] {
		out[idx[j]] = true
	}

	// A production-only safeguard that fires silently is unobservable, and
	// this project's own posture is that a single capture session cannot
	// reveal what only shows up weeks apart -- see roster_capture's rank
	// groups note in CLAUDE.md for the same argument reached elsewhere. A
	// nonzero drop count here is exactly the kind of signal that should not
	// wait for someone to go looking.
	if dropped := len(idx) - bestLen; dropped > 0 {
		slog.WarnContext(ctx, "ingest: dropped points seeds inconsistent with the ranking's own order",
			"seeds", len(idx), "kept", bestLen, "dropped", dropped)
	}

	return out
}
