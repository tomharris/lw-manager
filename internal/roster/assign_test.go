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
	if got[1].Member == 0 {
		t.Errorf("row 1 = %+v, but member 0 is already row 0's", got[1])
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
