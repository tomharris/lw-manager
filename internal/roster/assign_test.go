package roster_test

import (
	"testing"

	"github.com/tomharris/lw-manager/internal/roster"
)

func TestAssignPinsConfidentRowsBeforeWorkingTheResidual(t *testing.T) {
	// Row 0 is a clean match for member 1. Row 1 is a weak read whose own
	// member is the only one left once row 0 is pinned -- which is the whole
	// point: 66 is meaningless against a full roster and decisive against one
	// remaining candidate.
	scores := [][]int{
		{40, 100},
		{66, 30},
	}
	got := roster.Assign(scores, roster.DefaultResidual)
	if got[0].Member != 1 || got[0].Phase != roster.PhaseConfident {
		t.Errorf("row 0 = %+v, want member 1 resolved at phase 1", got[0])
	}
	if got[1].Member != 0 || got[1].Phase != roster.PhaseResidual {
		t.Errorf("row 1 = %+v, want member 0 resolved at phase 2", got[1])
	}
}

func TestAssignRefusesAnAmbiguousResidual(t *testing.T) {
	// Two free members within the margin of each other. Picking the higher by
	// a nose is exactly the false attribution the margin exists to prevent:
	// a queued row is recoverable, one member's score on another's row is not.
	scores := [][]int{{80, 75}}
	got := roster.Assign(scores, roster.DefaultResidual)
	if got[0].Member != -1 || got[0].Phase != roster.PhaseUnassigned {
		t.Errorf("row 0 = %+v, want unassigned: 80 against 75 is inside the 20-point margin", got[0])
	}
}

func TestAssignNeverGivesOneMemberTwoRows(t *testing.T) {
	// The pinned self row: the same member read cleanly at two screen
	// positions. One row takes them; the other must not.
	scores := [][]int{
		{100, 10},
		{98, 12},
	}
	got := roster.Assign(scores, roster.DefaultResidual)
	if got[0].Member != 0 {
		t.Fatalf("row 0 = %+v, want member 0", got[0])
	}
	// Asserted by PHASE, not merely by "not member 0". Row 1 comes back with
	// Member -1 whether the duplicate check ran or not -- unresolved and
	// duplicate-withheld look identical through Member alone -- so deleting
	// the entire duplicate loop from Assign left the old assertion passing.
	// PhaseDuplicate is the only observable that distinguishes them, and it
	// is the one that matters: a duplicate is withheld from phase 2, where an
	// ordinary unassigned row still competes.
	if got[1].Phase != roster.PhaseDuplicate {
		t.Errorf("row 1 = %+v, want PhaseDuplicate (member 0 is already row 0's)", got[1])
	}
	if got[1].Member != -1 {
		t.Errorf("row 1 = %+v, want Member -1", got[1])
	}
}

// Phase 1's margin refuses an exact tie. Row 0 scores identically against two
// FREE members, both well above AutoAccept: there is no evidence in the
// scores for choosing either, so the row must not be resolved to one on index
// order. Deleting the margin (or setting it back to 0, which disables the
// check entirely because top-second can never be negative) resolves row 0 to
// member 0 and writes a fact under a name picked by a coin flip.
//
// Row 1 exists so the tie is between two members that both stay free through
// phase 1 -- without it, one of them could be claimed first and the tie would
// dissolve into an ordinary single-candidate win.
func TestAssignRefusesAnExactTieBetweenTwoFreeMembers(t *testing.T) {
	scores := [][]int{
		{94, 94, 10},
		{10, 10, 100},
	}
	got := roster.Assign(scores, roster.DefaultResidual)
	if got[1].Member != 2 || got[1].Phase != roster.PhaseConfident {
		t.Fatalf("row 1 = %+v, want member 2 at phase 1", got[1])
	}
	if got[0].Phase == roster.PhaseConfident {
		t.Errorf("row 0 = %+v, but 94 vs 94 is a tie -- phase 1 must not pick one", got[0])
	}
}

// The other half, and the reason the test above is safe to write: a margin of
// 1 refuses ties and NOTHING else. A single point of separation is still a
// win, so the guard cannot be quietly costing rows that had real evidence
// behind them -- which is the failure mode this project has hit twice with
// fences fitted against what they admit rather than what they exclude.
func TestAssignAcceptsAOnePointWin(t *testing.T) {
	scores := [][]int{
		{95, 94, 10},
	}
	got := roster.Assign(scores, roster.DefaultResidual)
	if got[0].Member != 0 || got[0].Phase != roster.PhaseConfident {
		t.Errorf("row 0 = %+v, want member 0 at phase 1 (95 beats 94 by one)", got[0])
	}
}

func TestAssignHandlesMoreMembersThanRows(t *testing.T) {
	// Production is never square: the weekly ranking lists scorers only, so
	// the pool always holds members no row can claim. Recon measured 94
	// ranked rows against 96 alliance members.
	scores := [][]int{{100, 20, 15}}
	got := roster.Assign(scores, roster.DefaultResidual)
	if len(got) != 1 {
		t.Fatalf("got %d assignments, want one per row", len(got))
	}
	if got[0].Member != 0 {
		t.Errorf("row 0 = %+v, want member 0", got[0])
	}
}

func TestAssignLetsAConfidentPinDecideAWeakRow(t *testing.T) {
	// Row 1's best-scoring member is member 0 -- but member 0 is row 0's at
	// 100. Row 1's own score against member 0 (85) is deliberately kept below
	// AutoAccept (92): at or above it, the between-phases duplicate check
	// added for finding 3 would withhold this row from phase 2 entirely (see
	// TestAssignExcludesADuplicateFromTheResidualRatherThanLettingItStealAFreeMember
	// below) -- correctly, since a row scoring that close to an
	// already-claimed member is indistinguishable from a second sighting of
	// it. Below that line there is no such ambiguity, and phase 2 still owes
	// this row its best free candidate.
	scores := [][]int{
		{100, 30, 20},
		{85, 70, 20},
	}
	got := roster.Assign(scores, roster.DefaultResidual)
	if got[0].Member != 0 {
		t.Fatalf("row 0 = %+v, want member 0", got[0])
	}
	if got[1].Member != 1 || got[1].Phase != roster.PhaseResidual {
		t.Errorf("row 1 = %+v, want member 1 at phase 2", got[1])
	}
}

// Finding 3 (task-3-findings-round1.md): the pinned self row's member is
// pinned in phase 1, so without this check the second copy of that row would
// enter phase 2 as an ordinary unclaimed row -- free to claim a DIFFERENT
// member on the strength of a merely-adequate score, writing a fact onto a
// member whose row it never was. Row 1 here scores 100 for member 0 (already
// claimed by row 0) and would otherwise clear phase 2's floor/margin for
// member 2 at 70. It must come back unassigned and flagged as a duplicate,
// never holding member 2.
func TestAssignExcludesADuplicateFromTheResidualRatherThanLettingItStealAFreeMember(t *testing.T) {
	scores := [][]int{
		{100, 10, 5},
		{100, 10, 70},
		{5, 5, 5},
	}
	got := roster.Assign(scores, roster.DefaultResidual)
	if got[0].Member != 0 {
		t.Fatalf("row 0 = %+v, want member 0", got[0])
	}
	if got[1].Member != -1 || got[1].Phase != roster.PhaseDuplicate {
		t.Errorf("row 1 = %+v, want Member -1, Phase PhaseDuplicate -- not member 2", got[1])
	}
}

func TestAssignOnNoRows(t *testing.T) {
	if got := roster.Assign(nil, roster.DefaultResidual); len(got) != 0 {
		t.Errorf("Assign(nil) = %v, want empty", got)
	}
}
