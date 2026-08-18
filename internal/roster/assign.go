package roster

// Closed-set assignment: the VS weekly ranking is a matching between two sets
// of known size, not one independent lookup per row.
//
// Scoring each row against the whole roster and accepting at AutoAccept
// discards the constraint that a member appears at most once. Using it turns
// the tail of the capture into a much smaller problem: once the confident rows
// are pinned, the question stops being "does this read clear 92 against 86
// names" and becomes "among the members nobody has claimed, is one an
// unambiguous winner". Measured on capture 6, that is worth twelve members --
// a read scoring 60 is meaningless against 86 candidates and decisive against
// the four nobody has claimed.
//
// This file is deliberately pure: a score matrix in, one member index per row
// out. No OCR, no database, no images. Everything about how the scores were
// produced belongs to the caller.
const (
	// ResidualFloor and ResidualMargin govern the second pass. Both are
	// FITTED to capture 6 and are not principled constants: at margin 0 a
	// second misattribution appears under adversarial decoys, and at margin
	// >= 10 none does. Re-measure with `make probe-assign` on a later capture
	// and believe the newer number -- re-measure, do not re-reason.
	ResidualFloor  = 60
	ResidualMargin = 20
)

// Phases a row can be resolved in. The phase travels with the assignment
// because the caller must weight the two differently: a phase-1 match is a
// string that scored >= AutoAccept, and a phase-2 match is an elimination
// argument. Those are not the same evidence and must not carry the same
// confidence onto a leaderboard.
//
// PhaseDuplicate is neither: it is not a match at all, only a reason a row
// was withheld from phase 2. See Assign's doc comment for why detecting it
// between the phases, rather than after both, is load-bearing.
const (
	PhaseUnassigned = 0
	PhaseConfident  = 1
	PhaseResidual   = 2
	PhaseDuplicate  = 3
)

// AssignParams is one phase's acceptance rule.
type AssignParams struct {
	// Floor is the score a claim must reach.
	Floor int
	// Margin is how far the best free candidate must beat the next free one.
	// It is what stops the assignment picking a winner by a nose, and it is
	// the setting that measurably holds misattribution down.
	Margin int
}

// DefaultResidual is the shipped second-phase rule.
var DefaultResidual = AssignParams{Floor: ResidualFloor, Margin: ResidualMargin}

// Assignment is one row's outcome. Member is -1 when the row was not resolved.
type Assignment struct {
	Member int
	Score  int
	Phase  int
}

// Assign matches rows to members. scores[i][j] is row i's score against
// member j; the result has one entry per row.
//
// Two phases, one rule: a row may claim a member only when that member is the
// best FREE candidate for it, clears the phase's floor, and beats the next
// free candidate by the phase's margin. Phase 1 runs at AutoAccept with no
// margin -- today's bar, which pins the confident rows first -- and phase 2
// runs the caller's residual rule over what is left.
//
// Phase 2 is not a relaxed threshold. It is a different criterion, and a
// stricter one where it counts: it is conditioned on every confident row
// already being pinned, so it cannot be satisfied by a read that merely
// resembles a popular name. A member who has their own row is not available
// to steal somebody else's.
//
// Each phase repeatedly claims the globally highest-scoring eligible pair,
// which is a greedy approximation to a maximum-weight matching. Greedy is
// chosen over Hungarian deliberately, and not only because it is O(n^3) at
// n=86 and fits on a screen: an optimal matching maximizes the TOTAL and will
// happily displace a pair scoring 100 to raise it, which is not a trade this
// domain should ever make.
//
// Between the two claim passes, any row still unassigned is checked against
// the members phase 1 already claimed. A row scoring >= AutoAccept against an
// already-claimed member is a second sighting of that member -- the pinned
// self row, structurally -- and is marked PhaseDuplicate and withheld from
// phase 2 entirely, rather than left to compete for whatever else it
// resembles.
//
// This has to sit BETWEEN the phases, not after both. A row like that can
// also carry a middling score against some other, still-free member -- enough
// to look like an ordinary residual candidate once phase 2 runs. But a second
// sighting of a claimed member is not evidence about anyone else: it is
// evidence about the member phase 1 already resolved, full stop. Letting it
// compete in the residual turns a harmless duplicate into a misattribution --
// a fact written under a real, different member's name, for a row that was
// never theirs -- which is the one failure a review queue cannot undo. A row
// dropped as a duplicate loses nothing recoverable; a row that duplicates its
// way onto someone else's fact does.
func Assign(scores [][]int, residual AssignParams) []Assignment {
	out := make([]Assignment, len(scores))
	for i := range out {
		out[i] = Assignment{Member: -1, Phase: PhaseUnassigned}
	}
	if len(scores) == 0 {
		return out
	}

	memberCount := 0
	for _, row := range scores {
		if len(row) > memberCount {
			memberCount = len(row)
		}
	}
	rowTaken := make([]bool, len(scores))
	memTaken := make([]bool, memberCount)

	claim := func(p AssignParams, phase int) {
		for {
			bestScore, bestRow, bestMem := -1, -1, -1
			for i, row := range scores {
				if rowTaken[i] {
					continue
				}
				top, second, topJ := -1, -1, -1
				for j, s := range row {
					switch {
					case memTaken[j]:
					case s > top:
						top, second, topJ = s, top, j
					case s > second:
						second = s
					}
				}
				if topJ < 0 || top < p.Floor {
					continue
				}
				// A margin against nothing is vacuous: when only one member
				// is free there is no rival to beat, and refusing the row
				// would discard the strongest structural evidence there is.
				if second >= 0 && top-second < p.Margin {
					continue
				}
				if top > bestScore {
					bestScore, bestRow, bestMem = top, i, topJ
				}
			}
			if bestRow < 0 {
				return
			}
			rowTaken[bestRow], memTaken[bestMem] = true, true
			out[bestRow] = Assignment{Member: bestMem, Score: bestScore, Phase: phase}
		}
	}

	claim(AssignParams{Floor: AutoAccept, Margin: 0}, PhaseConfident)

	// See the doc comment above for why this runs between the phases: a row
	// that already matches a claimed member at AutoAccept must not be given
	// the chance to claim someone else's row on the strength of a lesser
	// score.
	for i, row := range scores {
		if rowTaken[i] {
			continue
		}
		for j, s := range row {
			if memTaken[j] && s >= AutoAccept {
				out[i] = Assignment{Member: -1, Phase: PhaseDuplicate}
				rowTaken[i] = true
				break
			}
		}
	}

	claim(residual, PhaseResidual)
	return out
}
