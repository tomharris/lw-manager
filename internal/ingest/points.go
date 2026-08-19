package ingest

import (
	"context"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
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
//
// This struct carried an AdjacentSeeds field for one fix round: closure
// (Lo > 0 && Hi < MaxInt64) alone does not imply a narrow window, so
// AdjacentSeeds additionally required the bracketing values to come from
// the immediate rank neighbours (known[i-1] and known[i+1]), demonstrated
// against a synthetic 5-row fixture where non-adjacent seeds closed a wide
// window over three wrong values.
//
// It was withdrawn, and the reason is worth keeping visible rather than
// just deleting the field: measured against the REAL capture (not only the
// synthetic counterexample that motivated it), adjacency cost two rows --
// ranks 38 and 76 -- and both were CORRECT. It prevented zero wrong values
// on this capture. That is the identical mistake made with the original
// pointsOrderConfidenceFloor: a fence justified against what it admits
// (the synthetic counterexample it correctly rejects) and never measured
// against what it excludes (the real rows it also rejects). Adjacent seeds
// do not guarantee a narrow window, and non-adjacent seeds do not imply a
// wide one -- it is simply the wrong proxy, in both directions. See
// vsRun.attributeRow's corroborated comment for what replaced it (a window
// WIDTH check) and the same honest limit repeated there.
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
// clearing withinBounds is not by itself proof of corroboration -- see the
// "corroborated" check in vsRun.attributeRow (vs.go), which additionally
// requires the window to be CLOSED on both sides before this is trusted as
// more than "the parser matched".
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

// trailingArtifactRe matches a trailing run of characters that are neither a
// digit nor a comma -- the shape of a crop that caught a few pixels of
// whatever sits just past the number: a stray space, an em-dash, a
// scoreboard glyph. It is anchored at the end ONLY -- see
// trimPointsArtifact's comment for why that restriction is the entire
// safety property, not a simplification of something bidirectional.
var trailingArtifactRe = regexp.MustCompile(`[^0-9,]+$`)

// trimPointsArtifact strips a trailing run of non-digit, non-comma
// characters from a points read, before ParsePoints ever sees it. Capture
// 6's rank 20 read "12,090,000 —": every digit is right and the value is
// lost only to a trailing space and em-dash that pointsRe correctly has no
// business admitting. This recovers the string those digits were already
// spelling; it constructs nothing, which is what lets it run unconditionally
// on every read rather than being fenced like repairPoints below.
//
// Trailing-only is not a stylistic choice; it is the whole safety argument,
// and it holds for two independent reasons:
//
//   - A crop that instead caught the START of the next row's points -- the
//     failure vsPointsSpec's charset comment describes at length -- ends in
//     a DIGIT ("12,090,000 8,671,806"), not a symbol. A trailing-only trim
//     does not touch it, so the embedded space still fails pointsRe and the
//     row still routes to review rather than writing the wrong row's value.
//   - A LEADING corruption -- "¢,609,299" -- must NEVER be trimmed.
//     Stripping "¢" produces a shorter, differently-grouped digit string
//     that is wrong by roughly a factor of ten, and nothing downstream would
//     know to doubt it -- it would parse cleanly and carry the read's own
//     confidence like any other clean read. Restricting this function to the
//     trailing end means there is no code path in it that could ever touch a
//     leading character, which is a stronger guarantee than "we didn't
//     intend to."
//
// The ordering bounds in points.go sit behind this as a second guard, the
// same defense-in-depth relationship pointsRe already has with the old
// charset whitelist -- but this trim is deliberately narrow enough that nothing
// here should need to lean on them.
func trimPointsArtifact(s string) string {
	return trailingArtifactRe.ReplaceAllString(s, "")
}

// repairPoints recovers a value from a read with exactly one non-digit in a
// digit position, by solving for the digit subject to the bounds.
//
// This is the only place in the pipeline that CONSTRUCTS a value rather than
// validating one, and it is fenced accordingly. It refuses unless the string
// is comma-grouped with exactly one damaged position, and unless the bounds
// admit exactly one digit there. Two admissible digits is not a near miss, it
// is a guess, and a guessed number on a leaderboard is the failure a review
// queue cannot undo.
//
// The repaired value still carries the read's own confidence and still points
// at the same crop of the same screenshot, so invariants #4 and #5 hold: it is
// an observation about pixels, narrowed by an ordering the same capture
// establishes.
//
// It never seeds a bound for another row, and that is not an extra check
// here -- it is structural. pointsBounds runs once, over every row's
// UNREPAIRED read, before the per-row loop that calls repairPoints even
// starts (see the seeding loop in IngestVS); a repaired value is produced
// too late in the pipeline to ever reach it. A constructed value must never
// define another row's window, and the ordering of the pipeline is what
// enforces that, not a flag threaded through this function.
func repairPoints(raw string, b pointsBound) (int64, bool) {
	t := strings.TrimSpace(raw)

	// Exactly one damaged position, and the damage must be where a digit
	// belongs -- a broken comma means the grouping is not intact and there is
	// no shape left to solve against.
	bad := -1
	for i, r := range t {
		switch {
		case r >= '0' && r <= '9', r == ',':
		case bad >= 0:
			return 0, false // a second damaged position
		default:
			bad = i
		}
	}
	if bad < 0 {
		return 0, false // nothing to repair; the caller should have parsed it
	}

	// bad is a BYTE offset from range, and the damaged rune is usually
	// multi-byte -- "¢" is two bytes and "€" is three -- so the tail has to
	// skip the whole rune. Slicing by bad+1 would leave a stray continuation
	// byte in the candidate, which ParsePoints rejects, and every digit would
	// then look inadmissible for a reason that has nothing to do with the
	// bounds.
	_, width := utf8.DecodeRuneInString(t[bad:])

	var found int64
	hits := 0
	for d := '0'; d <= '9'; d++ {
		cand := t[:bad] + string(d) + t[bad+width:]
		v, err := ParsePoints(cand)
		if err != nil || !withinBounds(v, b) {
			continue
		}
		found, hits = v, hits+1
		if hits > 1 {
			return 0, false // the bounds do not determine it; refuse
		}
	}
	if hits != 1 {
		return 0, false
	}
	return found, true
}
