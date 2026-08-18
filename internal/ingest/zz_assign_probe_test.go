//go:build m4probe

// The M4 assignment probe: does closed-set matching beat per-row thresholding,
// and at what false-attribution cost?
//
// This started as a throwaway spike and is committed for the reason
// zz_name_probe_test.go's own header gives for itself: the numbers that
// justified closed-set matching came from a harness like this, and rebuilding
// a harness like this once already cost a session.
//
// Question it answers: today's matcher scores each row independently against
// all 86 members and accepts at roster.AutoAccept. The VS ranking is not 86
// independent lookups — it is an assignment between two sets of known size
// where each member appears at most once. Does using that structure recover
// the tail the absolute threshold cannot reach, and at what false-attribution
// cost?
//
// It asserts nothing and is not a gate. Run it with:
//
//	make probe-assign
//	make probe-assign PROBE_ARGS='-probe.assignpsm=13'   # union PSM 7+13
//	make probe-assign PROBE_ARGS='-probe.assigndetail'
//
// Two modes exist to keep the headline honest, and both must be run, not just
// read about, before believing one: -probe.assignshuffle rotates the truth
// labels by one rank so every assignment is wrong by construction, and must
// report ~0 correct; -probe.assigndecoys=N pads the member set with N members
// one confusable substitution from a real name, because the gate's capture is
// square (86 rows, 86 members) and production is not. Neither is optional
// decoration: a forced assignment at floor 0 / margin 0 once produced a
// PERFECT result on this capture, which read as an overwhelming finding and
// was really an untested instrument, and only the shuffle run — reporting ~0
// correct instead of the same perfect score — proved the counters were
// measuring attribution at all.
//
// Ground truth comes from replaying IngestVS's own band-to-row arithmetic:
// contentY + (band.Y0 - regionTop), with the same geometric dedupe. Rank order
// is screen order, so deduped row i is exp.Rows[i]. That is the alignment
// production actually uses rather than a parallel guess at it, and if the
// deduped count does not equal the transcribed row count the probe says so and
// stops rather than scoring a misalignment.
package ingest

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
)

var (
	probeAssign = flag.Bool("probe.assign", false,
		"measure closed-set assignment against today's per-row threshold matching")
	probeAssignPSMs = flag.String("probe.assignpsm", "",
		"extra page-segmentation modes to union into each row's score vector, e.g. '13'; the shipped PSM always runs")
	probeAssignDetail = flag.Bool("probe.assigndetail", false,
		"print every row the assignment resolved that the threshold did not, and every row it got wrong")
	probeAssignShuffle = flag.Bool("probe.assignshuffle", false,
		"CANARY: rotate the truth labels by one rank so every assignment is wrong by construction; if this still reports zero wrong, the instrument is not measuring attribution")
	probeAssignDecoys = flag.Int("probe.assigndecoys", 0,
		"pad the member set with N adversarial decoys, one confusable substitution away from a real name; the gate's capture is square (86 rows, 86 members) and production is not (recon: 94 ranked rows, 96 alliance members)")
)

// decoyMembers builds N members that do not appear in the ranking, each one
// confusable substitution away from a real name.
//
// The squareness of the gate's capture is the assignment's biggest unearned
// advantage: expected.yaml transcribes only the ranked rows, and probeMembers
// builds the member list from those same rows, so every member has a row and
// every row has a member. Production never looks like that -- the weekly
// ranking lists scorers only, and recon measured 94 ranked rows against 96
// alliance members, so the residual pool always holds members who cannot be
// claimed by anything.
//
// Random names would not test this. A decoy is only dangerous if it competes,
// so each one is a real name with a single character swapped for one OCR
// interchanges it with -- exactly the near-neighbour that makes a misread
// ambiguous. If the assignment survives these it will survive arbitrary
// non-scorers, which makes this a lower bound rather than a simulation of the
// real ones (whose names this fixture does not record).
func decoyMembers(real []roster.Member, n int) []roster.Member {
	swaps := []struct{ from, to rune }{
		{'0', 'o'}, {'1', 'l'}, {'5', 's'}, {'8', 'b'}, {'a', 'e'},
		{'o', '0'}, {'l', '1'}, {'s', '5'}, {'i', 'l'}, {'e', 'c'},
	}
	out := make([]roster.Member, 0, n)
	nextID := int64(len(real) + 1)
	for i := 0; len(out) < n && i < len(real)*len(swaps); i++ {
		src := real[i%len(real)]
		sw := swaps[(i/len(real))%len(swaps)]
		mutated := []rune(src.Name)
		changed := false
		for j, r := range mutated {
			if r == sw.from || r == sw.from-32 {
				mutated[j] = sw.to
				changed = true
				break
			}
		}
		if !changed {
			continue
		}
		out = append(out, roster.Member{ID: nextID, Name: string(mutated)})
		nextID++
	}
	return out
}

// assignRow is one deduped ranking row: where it came from, what was read, and
// how every member scored against that read.
type assignRow struct {
	Rank  int    // 1-based screen position, which is also the rank
	Truth string // the hand-transcribed name at that rank
	Text  string // the best read (the one that produced the top score)
	// Score is indexed by the same order as the member slice, so
	// Score[j] is how member j scored against this row.
	Score []int
}

func TestM4AssignProbe(t *testing.T) {
	if !*probeAssign {
		t.Skip("spike instrument; enable with -probe.assign")
	}
	ctx := context.Background()

	exp := loadProbeCapture(t)
	engine := ocr.NewTesseractEngine()
	if !engine.Available() {
		t.Skip("tesseract is not on PATH; see the gate's skip message")
	}
	frames := loadProbeFrames(t, ctx, exp)
	members := probeMembers(exp)
	if *probeAssignDecoys > 0 {
		decoys := decoyMembers(members, *probeAssignDecoys)
		members = append(members, decoys...)
		t.Logf("padded the member set with %d adversarial decoys (e.g. %q, %q); %d members against %d rows",
			len(decoys), decoys[0].Name, decoys[len(decoys)-1].Name, len(members), len(exp.Rows))
	}

	rows := readAssignRows(ctx, t, engine, frames, exp, members)
	if *probeAssignShuffle {
		// Rotating the truth by one rank makes every correct assignment
		// impossible. Anything but ~0 correct here means Correct/Wrong are not
		// comparing what they claim to.
		for i := range rows {
			rows[i].Truth = exp.Rows[(i+1)%len(exp.Rows)].Name
		}
		t.Log("CANARY: truth labels rotated by one rank; expect ~0 correct and ~86 wrong")
	}
	if len(rows) != len(exp.Rows) {
		t.Fatalf("geometric dedupe produced %d rows but the capture transcribes %d; "+
			"the band-to-rank alignment this probe depends on does not hold, so every number below would be scored against the wrong name",
			len(rows), len(exp.Rows))
	}
	t.Logf("assign probe: %d frames, %d deduped rows, %d members, reads at PSM %s",
		len(frames), len(rows), len(members), assignPSMLabel(t, engine.PSM))

	// The baseline: exactly what processRow does today. Each row takes its own
	// top-scoring member over the WHOLE roster, accepts at AutoAccept, and a
	// member already scored is dropped rather than reconsidered.
	base := scoreThreshold(rows, members)
	t.Logf("--- baseline (per-row, >= AutoAccept %d, arrival-order dedupe) ---", roster.AutoAccept)
	t.Logf("  correct %2d/%d   wrong %d   unassigned %d",
		base.Correct, len(rows), base.Wrong, len(rows)-base.Correct-base.Wrong)

	// The assignment: phase 1 claims the confident rows at today's bar, then
	// phase 2 works the residual, where the question is no longer "does this
	// read clear 92 against 86 names" but "among the members nobody has
	// claimed, is one an unambiguous winner".
	t.Logf("--- closed-set assignment (phase 1 at %d, then residual floor/margin) ---", roster.AutoAccept)
	t.Logf("  %-6s %-7s  %-8s %-6s %-11s", "floor", "margin", "correct", "wrong", "unassigned")
	// The last row of the grid is a canary, not a candidate setting: floor 0 /
	// margin 0 forces every row to claim its best free member however bad the
	// read. If THAT reports zero wrong assignments, the instrument is not
	// measuring attribution at all and no number above it means anything.
	dupCount := -1
	for _, floor := range []int{75, 70, 65, 60, 50, 40, 0} {
		for _, margin := range []int{30, 20, 15, 10, 0} {
			res := scoreAssignment(rows, members, floor, margin)
			// Duplicate detection sits between phase 1 (fixed at AutoAccept,
			// margin 0) and phase 2, so it never depends on this cell's
			// floor/margin -- res.Duplicate is the same number on every
			// iteration of this loop. Captured once rather than logged 35
			// times over.
			if dupCount < 0 {
				dupCount = res.Duplicate
			}
			t.Logf("  %-6d %-7d  %2d/%-6d %-6d %-11d",
				floor, margin, res.Correct, len(rows), res.Wrong, len(rows)-res.Correct-res.Wrong-res.Duplicate)
		}
	}
	t.Logf("  duplicates: %d (rows scoring >= AutoAccept against a member phase 1 already claimed; "+
		"withheld from the residual phase, counted separately because they are neither correct nor wrong)", dupCount)

	if *probeAssignDetail {
		// One setting, spelled out per row. The grid says how many; this says
		// which, and a wrong assignment is the number that decides whether any
		// of this is safe -- it writes one member's score onto another's row,
		// the one failure a review queue cannot undo.
		res := scoreAssignment(rows, members, 60, 20)
		printAssignDetail(t, rows, members, base, res)
	}
}

// readAssignRows replays IngestVS's frame walk -- offset accumulation and
// geometric dedupe -- and reads the name field of every surviving row.
//
// The dedupe decision itself is shared, not reimplemented: it goes through
// vsRowCursor (internal/ingest/vs.go), the type Task 2 extracted from this
// same arithmetic for exactly this reason. Everything around it stays local
// -- the probe cannot call readVSRows, which resolves frames through
// []db.CaptureFrame and the database, and this probe has decoded images and
// no database at all.
//
// The dedupe is the point. The name probe next door deliberately scores every
// band of every frame, because a member visible in three overlapping frames
// gets three chances there and a single bad band should be visibly different
// from a systematic miss. An assignment cannot work that way: it needs one row
// per rank, or a member could be "assigned" to a duplicate of somebody else's
// row and the count would mean nothing.
func readAssignRows(ctx context.Context, t *testing.T, engine *ocr.TesseractEngine, frames []probeLoadedFrame, exp probeCapture, members []roster.Member) []assignRow {
	t.Helper()

	psms := []int{engine.PSM}
	if *probeAssignPSMs != "" {
		psms = append(psms, parsePSMList(t, *probeAssignPSMs)...)
	}

	offsetBySeq := make(map[int]int, len(exp.Frames))
	for _, f := range exp.Frames {
		offsetBySeq[f.Seq] = f.OffsetPx
	}

	ing := &Ingester{engine: engine}
	var out []assignRow
	cursor := newVSRowCursor()

	for _, f := range frames {
		cursor.nextFrame(offsetBySeq[f.Seq])

		bands, err := SegmentRows(f.Img, vsListRegion, vsRowPitch)
		if err != nil {
			t.Fatalf("segmenting frame %d: %v", f.Seq, err)
		}
		regionTop := int(vsListRegion.Y1 * float64(f.Img.Bounds().Dy()))

		for _, band := range bands {
			if !cursor.accept(band, regionTop) {
				continue // geometric duplicate, exactly as IngestVS drops it
			}

			rect := fieldRect(band, f.Img, vsNameXFrac0, vsNameXFrac1, vsNameYFrac0, vsNameYFrac1)
			row := assignRow{Rank: len(out) + 1, Score: make([]int, len(members))}
			if row.Rank <= len(exp.Rows) {
				row.Truth = exp.Rows[row.Rank-1].Name
			}

			// Every PSM contributes to the same score vector, per member,
			// taking the better. Union rather than pick-one: the two modes'
			// miss sets are disjoint in four places on this capture, and a
			// mode that reads a row badly simply fails to raise that row's
			// scores.
			bestTop := -1
			for _, psm := range psms {
				primary := readPlan{spec: vsNameSpec, opts: vsNameOptions}
				primary.spec.PSM = psm
				read, err := ing.readFieldWithRetry(ctx, f.Img, rect, primary, vsNameRetry)
				if err != nil {
					t.Fatalf("reading frame %d band %d: %v", f.Seq, band.Y0, err)
				}
				text := strings.TrimSpace(read.Text)
				if text == "" {
					continue
				}
				top := 0
				for j, m := range members {
					s := roster.TokenSetRatio(text, m.Name)
					if s > row.Score[j] {
						row.Score[j] = s
					}
					if s > top {
						top = s
					}
				}
				if top > bestTop {
					bestTop, row.Text = top, text
				}
			}
			out = append(out, row)
		}
	}
	return out
}

// assignResult counts the outcomes that matter. Wrong is the one to read
// first: an unassigned row is recoverable from the review queue, and a row
// assigned to the wrong member is not.
type assignResult struct {
	Correct int
	Wrong   int
	// Duplicate counts rows roster.Assign marked PhaseDuplicate: a row that
	// scored >= AutoAccept against a member phase 1 already claimed, withheld
	// from phase 2 rather than left to compete for someone else's row. It is
	// neither correct nor wrong -- see scoreAssignment's doc comment -- and is
	// reported separately rather than folded into an "unassigned" count that
	// would otherwise show only an unexplained bump with nothing saying why.
	// scoreThreshold never sets this: its arrival-order dedupe has no
	// equivalent phase split to detect one.
	Duplicate int
	// By is the member index each row was assigned, or -1.
	By []int
}

// scoreThreshold reproduces processRow: per row, the top candidate over the
// whole roster, accepted at AutoAccept, with a member that already matched
// dropped rather than reconsidered.
func scoreThreshold(rows []assignRow, members []roster.Member) assignResult {
	res := assignResult{By: make([]int, len(rows))}
	taken := make([]bool, len(members))
	for i := range rows {
		res.By[i] = -1
		best, bestJ := 0, -1
		for j := range members {
			if rows[i].Score[j] > best {
				best, bestJ = rows[i].Score[j], j
			}
		}
		if bestJ < 0 || best < roster.AutoAccept || taken[bestJ] {
			continue
		}
		taken[bestJ] = true
		res.By[i] = bestJ
		if members[bestJ].Name == rows[i].Truth {
			res.Correct++
		} else {
			res.Wrong++
		}
	}
	return res
}

// scoreAssignment runs the production two-phase matcher, roster.Assign, over
// this capture's rows and scores its output against the hand-transcribed
// truth.
//
// This used to reimplement the two-phase claim loop inline, before
// internal/roster/assign.go shipped the real thing. That reimplementation is
// gone: an instrument that reimplements the algorithm it is measuring
// silently drifts from what IngestVS actually does, which is exactly the
// failure CLAUDE.md records for the fallback-sweep probe whose retry was
// firing inside the engine before the probe ever saw an empty string. floor
// and margin become roster.AssignParams for the RESIDUAL phase only --
// roster.Assign always runs phase 1 at AutoAccept with no margin internally,
// same as this function's old inline `claim(roster.AutoAccept, 0)` did.
//
// roster.Assign also reports PhaseDuplicate rows: a row that scored >=
// AutoAccept against a member phase 1 already claimed, withheld from phase 2
// rather than left to compete for someone else's identity (Member is -1, same
// as an unresolved row). This probe counts a duplicate as neither correct nor
// wrong -- it was never attributed to anyone, so there is nothing to compare
// against the truth label -- but tallies it separately in res.Duplicate
// rather than leaving it silently inside "unassigned", because this
// instrument is meant to outlive capture 6: capture 6 is square and its
// region already excludes the pinned self row, so the plain run reports zero
// duplicates, but a later capture with a pinned self row visible should show
// a nonzero count with a name, not just an unexplained bump in the
// unassigned column. Production counts duplicates the same way
// (VSResult.Duplicates), so this is what that field measures here.
// -probe.assigndecoys pads the member set rather than the row set, so it
// cannot manufacture a duplicate either.
func scoreAssignment(rows []assignRow, members []roster.Member, floor, margin int) assignResult {
	scores := make([][]int, len(rows))
	for i := range rows {
		scores[i] = rows[i].Score
	}
	assignments := roster.Assign(scores, roster.AssignParams{Floor: floor, Margin: margin})

	res := assignResult{By: make([]int, len(rows))}
	for i, a := range assignments {
		res.By[i] = a.Member
		if a.Member < 0 {
			if a.Phase == roster.PhaseDuplicate {
				res.Duplicate++
			}
			continue // PhaseUnassigned or PhaseDuplicate: not attributed to anyone
		}
		if members[a.Member].Name == rows[i].Truth {
			res.Correct++
		} else {
			res.Wrong++
		}
	}
	return res
}

func printAssignDetail(t *testing.T, rows []assignRow, members []roster.Member, base, res assignResult) {
	t.Helper()
	t.Log("--- rows the assignment resolved that the threshold did not (floor 60, margin 20) ---")
	for i := range rows {
		if base.By[i] >= 0 || res.By[i] < 0 {
			continue
		}
		got := members[res.By[i]].Name
		verdict := "ok"
		if got != rows[i].Truth {
			verdict = "WRONG"
		}
		t.Logf("  rank %3d  truth %-24q read %-24q -> %-24q %s (score %d)",
			rows[i].Rank, rows[i].Truth, rows[i].Text, got, verdict, rows[i].Score[res.By[i]])
	}
	t.Log("--- rows still unassigned ---")
	for i := range rows {
		if res.By[i] >= 0 {
			continue
		}
		bestJ, best := -1, -1
		for j := range members {
			if rows[i].Score[j] > best {
				best, bestJ = rows[i].Score[j], j
			}
		}
		name := ""
		if bestJ >= 0 {
			name = members[bestJ].Name
		}
		t.Logf("  rank %3d  truth %-24q read %-24q best %q at %d",
			rows[i].Rank, rows[i].Truth, rows[i].Text, name, best)
	}
}

func assignPSMLabel(t *testing.T, def int) string {
	t.Helper()
	psms := []string{fmt.Sprint(def)}
	if *probeAssignPSMs != "" {
		for _, p := range parsePSMList(t, *probeAssignPSMs) {
			psms = append(psms, fmt.Sprint(p))
		}
	}
	sort.Strings(psms)
	return strings.Join(psms, "+")
}
