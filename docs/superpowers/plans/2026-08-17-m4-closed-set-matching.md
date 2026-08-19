# M4 Closed-Set Matching Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Take `make gate-m4` from 65/86 rows to ≥82/86 (95%) on a cold run, by matching the VS ranking as a closed-set assignment instead of 86 independent lookups, and by validating the points field against the ranking's own descending order.

**Architecture:** `IngestVS` becomes two-pass — pass 1 reads every deduped row's name and points and writes nothing; pass 2 assigns rows to members globally, then writes facts and review rows. The assignment itself is a pure function in `internal/roster` with no OCR, no database and no images, so it is tested device-free. The points field gains a monotonicity check that both rejects out-of-order values and corroborates in-order ones.

**Tech Stack:** Go, `CGO_ENABLED=0`, tesseract CLI as a subprocess, pgx, Postgres 16 on host port 5433.

**Spec:** `docs/superpowers/specs/2026-08-17-m4-closed-set-matching-design.md` — read it before Task 1. The plan argues from it and does not restate its reasoning.

## Global Constraints

- **`CGO_ENABLED=0`, always.** Enforced by `make verify-nocgo`. No gocv, no gosseract, no onnxruntime_go.
- **`go test ./...` must pass with no emulator, no adb, no Docker.** Anything needing more goes behind a build tag.
- **No absolute pixel coordinates outside a `Transport` implementation.** Everything upstream speaks `transport.Norm`.
- **Facts are append-only.** Corrections supersede via `superseded_by`; nothing is mutated in place. Use `UpsertFact`, never `InsertFact`, on both ingest routes.
- **Every OCR-derived number carries a confidence and a screenshot reference.** Low-confidence reads go to the review queue, never to a leaderboard. `factConfidenceGate = 0.80` is enforced at write time.
- `context.Context` is the first parameter of anything that does I/O.
- Wrap errors with `%w` and enough context to locate the failure: which device, which account, which key.
- All logging goes through `log/slog` to **stderr**; CLI results to stdout.
- Sentinel errors compared with `errors.Is`/`errors.As`, never by string.
- **Do not touch `fixtures/m4gate/expected.yaml`.** It is 86 rows read by eye off 21 frames and cannot be regenerated. If a task seems to need it changed, the task is wrong.

## File Structure

| file | responsibility |
|---|---|
| `internal/roster/assign.go` | **NEW.** Pure closed-set assignment: score matrix in, one member index per row out. No I/O of any kind. |
| `internal/roster/assign_test.go` | **NEW.** Table-driven, device-free. |
| `internal/roster/confusable.go` | Modified: `ClosestPairScore` doc updated for whole-roster use. |
| `internal/ingest/vs.go` | Modified: two-pass `IngestVS`, `vsRow`, assignment-based attribution, residual confidence, duplicate counting. |
| `internal/ingest/points.go` | **NEW.** Monotonicity bounds for the points field: reject, corroborate, guarded retry. |
| `internal/ingest/points_test.go` | **NEW.** Bounds arithmetic against synthetic sequences. |
| `internal/ingest/zz_assign_probe_test.go` | **NEW (committed).** The measuring instrument, with its canary and decoy padding. |
| `Makefile` | Modified: `probe-assign` target. |

---

## Task 1: The pure assignment

**Files:**
- Create: `internal/roster/assign.go`
- Test: `internal/roster/assign_test.go`

**Interfaces:**
- Consumes: `roster.AutoAccept` (existing, = 92).
- Produces:
  ```go
  const (ResidualFloor = 60; ResidualMargin = 20)
  const (PhaseUnassigned = 0; PhaseConfident = 1; PhaseResidual = 2)
  type AssignParams struct{ Floor, Margin int }
  var DefaultResidual = AssignParams{Floor: ResidualFloor, Margin: ResidualMargin}
  type Assignment struct{ Member, Score, Phase int }
  func Assign(scores [][]int, residual AssignParams) []Assignment
  ```
  `scores[i][j]` is row *i*'s score against member *j*. The returned slice has one entry per row; `Member` is -1 when unassigned.

- [ ] **Step 1: Write the failing tests**

Create `internal/roster/assign_test.go`:

```go
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
	// 100. This is the "LOST" case measured on capture 6, where 2Rule's row
	// lost to B52RN10 at 100 while B52RN10 had its own row elsewhere.
	scores := [][]int{
		{100, 30, 20},
		{95, 70, 20},
	}
	got := roster.Assign(scores, roster.DefaultResidual)
	if got[0].Member != 0 {
		t.Fatalf("row 0 = %+v, want member 0", got[0])
	}
	if got[1].Member != 1 || got[1].Phase != roster.PhaseResidual {
		t.Errorf("row 1 = %+v, want member 1 at phase 2", got[1])
	}
}

func TestAssignOnNoRows(t *testing.T) {
	if got := roster.Assign(nil, roster.DefaultResidual); len(got) != 0 {
		t.Errorf("Assign(nil) = %v, want empty", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/roster/ -run TestAssign -v`
Expected: FAIL — `undefined: roster.Assign`

- [ ] **Step 3: Implement**

Create `internal/roster/assign.go`:

```go
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
const (
	PhaseUnassigned = 0
	PhaseConfident  = 1
	PhaseResidual   = 2
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
	claim(residual, PhaseResidual)
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/roster/ -v`
Expected: PASS, including the pre-existing normalize/match/confusable tests.

- [ ] **Step 5: Commit**

```bash
git add internal/roster/assign.go internal/roster/assign_test.go
git commit -m "Match the VS ranking as a closed set, not 86 lookups

Scoring each row against the whole roster and accepting at AutoAccept
discards the constraint that a member appears at most once. Using it turns
the tail into a much smaller problem: once the confident rows are pinned the
question stops being whether a read clears 92 against 86 names and becomes
whether one unclaimed member is an unambiguous winner. A read scoring 60 is
meaningless against 86 candidates and decisive against four.

Two phases share one rule, so the residual pass is not a relaxed threshold but
a different criterion -- conditioned on every confident row already being
pinned, it cannot be satisfied by a read that merely resembles a popular name.
The margin is what does the safety work: measured under adversarial decoys, a
margin of zero admits a second misattribution and anything from ten up does
not.

Greedy rather than Hungarian, and not only for the line count. An optimal
matching maximizes the total and will displace a pair scoring 100 to raise it,
which is not a trade this domain should make.

Pure by construction -- a score matrix in, member indices out, no OCR, no
database, no images -- so it tests with nothing running."
```

---

## Task 2: Two-pass `IngestVS`, behaviour preserving

**Files:**
- Modify: `internal/ingest/vs.go:288-437` (`IngestVS`), `internal/ingest/vs.go:439-507` (`processRow`)
- Test: existing `internal/ingest/vs_test.go` is the gate; no new tests in this task.

**Interfaces:**
- Consumes: `db.CaptureFrame{ID, CaptureID, Seq, ScreenshotID, OffsetPx, GroupKey}`, `(*Ingester).loadFrame`, `(*Ingester).readField`, `(*Ingester).readFieldWithRetry`, `SegmentRows`, `fieldRect`, `RowBand`, `roster.Rank`.
- Produces: `type vsRow struct{ScreenshotID int64; Band RowBand; NameText, PointsText string; PointsConf float64; Scores []int}`, `func (i *Ingester) readVSRows(ctx context.Context, frames []db.CaptureFrame, members []roster.Member) ([]vsRow, error)`, `func candidatesFor(row vsRow, members []roster.Member) []roster.Candidate`, `func (run *vsRun) attributeRow(ctx context.Context, i *Ingester, row vsRow, members []roster.Member, scored map[int64]bool) (bool, error)`.

**This task changes no behaviour.** It is a pure restructure whose test is that the existing suite and the gate produce identical numbers. Do not fold Task 3's logic in early — the point of the split is that a reviewer can reject the restructure without rejecting the algorithm.

**Critical compatibility note:** `ocr.FakeEngine` returns queued results in call order, and every VS test builds that queue as `name, points, name, points, ...` per row. `readVSRows` must read the name field then the points field for each band, in band order, exactly as `processRow` did, or every fixture desynchronises.

- [ ] **Step 1: Add the row type and pass 1**

In `internal/ingest/vs.go`, add `"sort"` to the imports, and add above `processRow`:

```go
// vsRow is one deduped ranking row: where it came from and what both fields
// read, held until every row has been read.
//
// Assignment cannot be streamed -- no row may be attributed until every row's
// scores are known -- so the frame walk stops writing anything and accumulates
// these instead. That also strengthens invariant #2 rather than straining it:
// a crash midway through the walk now leaves nothing written, where it used to
// leave a partial set of facts.
type vsRow struct {
	ScreenshotID int64
	Band         RowBand
	NameText     string
	PointsText   string
	PointsConf   float64
	// Scores is this row's name score against every member, indexed the same
	// as the members slice IngestVS built. It comes from roster.Rank, so
	// member aliases are already folded in and the assignment never has to
	// know they exist.
	Scores []int
}

// vsRowCursor replays the geometric dedupe across a capture's frames.
//
// It is a type rather than four local variables so the probes can share it.
// An instrument that reimplements the dedupe drifts from what IngestVS does,
// and then reports a row set production never sees -- which is the failure
// mode CLAUDE.md records for the probe whose retry was happening inside the
// engine before the probe ever saw it.
type vsRowCursor struct {
	contentY int
	lastRowY int
	havePrev bool
}

func newVSRowCursor() *vsRowCursor { return &vsRowCursor{lastRowY: -1} }

// nextFrame advances to a new frame, accumulating its measured scroll offset.
func (c *vsRowCursor) nextFrame(offsetPx int) {
	if c.havePrev {
		c.contentY += offsetPx
	}
	c.havePrev = true
}

// accept reports whether a band is a new row rather than a geometric
// duplicate of the one before it, and advances the cursor when it is.
func (c *vsRowCursor) accept(band RowBand, regionTop int) bool {
	rowY := c.contentY + (band.Y0 - regionTop)
	if c.lastRowY >= 0 && rowY <= c.lastRowY+vsRowPitch/2 {
		return false
	}
	c.lastRowY = rowY
	return true
}

// readVSRows is pass 1: walk the frames, drop geometric duplicates, and read
// both fields of every surviving row. It writes nothing.
//
// The field order -- name, then points, per band -- is load-bearing for the
// tests: ocr.FakeEngine returns queued results in call order and every VS
// fixture builds that queue name-then-points per row.
func (i *Ingester) readVSRows(ctx context.Context, frames []db.CaptureFrame, members []roster.Member) ([]vsRow, error) {
	idxByID := make(map[int64]int, len(members))
	for n, m := range members {
		idxByID[m.ID] = n
	}

	var rows []vsRow
	cursor := newVSRowCursor()

	for _, frame := range frames {
		img, err := i.loadFrame(ctx, frame.ScreenshotID)
		if err != nil {
			return nil, fmt.Errorf("ingest: loading screenshot %d: %w", frame.ScreenshotID, err)
		}
		cursor.nextFrame(frame.OffsetPx)

		bands, err := SegmentRows(img, vsListRegion, vsRowPitch)
		if err != nil {
			return nil, fmt.Errorf("ingest: segmenting screenshot %d: %w", frame.ScreenshotID, err)
		}
		regionTop := int(vsListRegion.Y1 * float64(img.Bounds().Dy()))

		for _, band := range bands {
			if !cursor.accept(band, regionTop) {
				continue // geometric duplicate; OCR never runs on it
			}

			nameRes, err := i.readFieldWithRetry(ctx, img,
				fieldRect(band, img, vsNameXFrac0, vsNameXFrac1, vsNameYFrac0, vsNameYFrac1),
				vsName, vsNameRetry)
			if err != nil {
				return nil, err
			}
			pointsRes, err := i.readField(ctx, img,
				fieldRect(band, img, vsPointsXFrac0, vsPointsXFrac1, vsPointsYFrac0, vsPointsYFrac1),
				vsPointsSpec, vsPointsOptions)
			if err != nil {
				return nil, err
			}

			scores := make([]int, len(members))
			for _, c := range roster.Rank(nameRes.Text, members) {
				scores[idxByID[c.MemberID]] = c.Score
			}
			rows = append(rows, vsRow{
				ScreenshotID: frame.ScreenshotID,
				Band:         band,
				NameText:     nameRes.Text,
				PointsText:   pointsRes.Text,
				PointsConf:   pointsRes.Confidence,
				Scores:       scores,
			})
		}
	}
	return rows, nil
}

// candidatesFor rebuilds the ranked candidate list from a row's stored score
// vector, so a review row can still record what the matcher was choosing
// between without pass 1 carrying a second copy of it.
func candidatesFor(row vsRow, members []roster.Member) []roster.Candidate {
	out := make([]roster.Candidate, 0, len(members))
	for j, m := range members {
		out = append(out, roster.Candidate{MemberID: m.ID, Name: m.Name, Score: row.Scores[j]})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out
}
```

- [ ] **Step 2: Replace `processRow` with `attributeRow`**

Delete `processRow` (`internal/ingest/vs.go:439-507`) and put in its place:

```go
// attributeRow routes one already-read row to a fact, a duplicate no-op, or
// the review queue. It never creates a member: the roster route is the only
// writer of that table, so a name that matches nothing here goes to review,
// full stop -- minting a member from a misread ranking row would corrupt the
// very count reconciliation depends on.
//
// Returns whether the row's name auto-accepted a match at all, independent of
// whether that member had already been scored this capture.
func (run *vsRun) attributeRow(ctx context.Context, i *Ingester, row vsRow, members []roster.Member, scored map[int64]bool) (bool, error) {
	candidates := candidatesFor(row, members)
	switch {
	case len(candidates) > 0 && candidates[0].Score >= roster.AutoAccept:
		memberID := candidates[0].MemberID
		if scored[memberID] {
			// Same member, a different screen position: the pinned self row
			// (or a genuine bug surfacing elsewhere). Deduplicate by member
			// id -- do not write a second fact for it.
			return true, nil
		}
		scored[memberID] = true
		run.res.Matched++

		matchNorm := float64(candidates[0].Score) / 100.0
		points, perr := ParsePoints(row.PointsText)
		if perr != nil {
			return true, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.PointsText, nil, "unparseable_points", 0)
		}
		conf := min(matchNorm, row.PointsConf)
		if conf < factConfidenceGate {
			return true, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.PointsText, nil, "low_confidence_points", conf)
		}
		// UpsertFact, not InsertFact, for the reason IngestRoster's writeFacts
		// documents at length: observed_at is pinned to the capture's own
		// started_at, so re-running ingest over this same capture recomputes
		// the identical key and a plain INSERT rejects it -- which is not
		// hypothetical, since resolving a review tells the operator to ingest
		// the capture again.
		if _, _, err := i.store.UpsertFact(ctx, db.Fact{
			MemberID: memberID, Metric: "vs_points", Value: float64(points),
			ObservedAt: run.observedAt, PeriodKey: run.periodKey,
			Source: "ocr:vs_ranking", ScreenshotID: row.ScreenshotID,
			Confidence: conf,
		}); err != nil {
			return true, fmt.Errorf("ingest: writing vs_points fact for member %d: %w", memberID, err)
		}
		return true, nil

	case len(candidates) > 0 && candidates[0].Score >= roster.ReviewFloor:
		return false, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.NameText, candidates, "ambiguous_name_match", 0)

	default:
		return false, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.NameText, candidates, "no_confident_match", 0)
	}
}
```

- [ ] **Step 3: Rewire `IngestVS`'s frame loop**

In `internal/ingest/vs.go`, replace the whole frame-walk block (from `scored := map[int64]bool{}` down to the closing brace of the `for _, frame := range frames` loop, `vs.go:330-372`) with:

```go
	scored := map[int64]bool{}

	rows, err := i.readVSRows(ctx, frames, members)
	if err != nil {
		return VSResult{}, err
	}
	totalParsed := len(rows)
	lastFrameShotID := int64(0)
	if len(frames) > 0 {
		lastFrameShotID = frames[len(frames)-1].ScreenshotID
	}

	matchedRowCount := 0
	for _, row := range rows {
		matched, err := run.attributeRow(ctx, i, row, members, scored)
		if err != nil {
			return VSResult{}, err
		}
		if matched {
			matchedRowCount++
		} else {
			run.res.Unidentified++
		}
	}
```

Delete the now-unused `var matchedRowCount, totalParsed int`, `var lastFrameShotID int64`, `contentY, lastRowY := 0, -1` and `havePrev := false` declarations that preceded the old loop.

- [ ] **Step 4: Run the unit tests**

Run: `go test ./internal/ingest/ -v`
Expected: PASS, every test, with no changes to any test file. `TestIngestVSDeduplicatesThePinnedSelfRow` still logs its `geometric_matches=6 identity_matches=5` warning — the cross-check is removed in Task 3, not here.

- [ ] **Step 5: Run the gate and confirm the number is unchanged**

Run: `make gate-m4`
Expected: FAIL with **exactly** `65/86 rows within 1%, matched=71 queued=21`. A different number means the restructure changed behaviour and must be fixed before Task 3 — a refactor that moves the number has hidden a bug inside a diff nobody will re-read.

- [ ] **Step 6: Commit**

```bash
git add internal/ingest/vs.go
git commit -m "Read every VS row before attributing any of them

Assignment cannot be streamed: no row may be attributed until every row's
scores are known. So the frame walk splits in two -- pass 1 reads both fields
of every deduped row and writes nothing, pass 2 attributes.

Behaviour is unchanged and the gate still reports 65/86 with matched=71,
which is the point of doing this as its own commit: the algorithm change
lands next, against a restructure already proved inert.

The field order within a band, name then points, is load-bearing rather than
incidental. ocr.FakeEngine returns queued results in call order and every VS
fixture builds that queue name-then-points per row, so reordering the two
reads desynchronises every test in the package.

Two-pass also strengthens invariant #2: a crash midway through the frame walk
now leaves nothing written, where it used to leave a partial set of facts."
```

---

## Task 3: Attribute by assignment

**Files:**
- Modify: `internal/ingest/vs.go` (`VSResult`, `IngestVS` pass 2, `attributeRow`)
- Test: `internal/ingest/vs_test.go`

**Interfaces:**
- Consumes: `roster.Assign`, `roster.DefaultResidual`, `roster.Assignment`, `roster.PhaseResidual`, `roster.PhaseConfident` (Task 1); `vsRow`, `candidatesFor` (Task 2).
- Produces: `VSResult.Duplicates int`; `func scoreMatrix(rows []vsRow) [][]int`; `attributeRow` gains an `a roster.Assignment` parameter and a `dup bool` parameter, becoming `func (run *vsRun) attributeRow(ctx context.Context, i *Ingester, row vsRow, a roster.Assignment, dup bool, members []roster.Member) (bool, error)`.

- [ ] **Step 1: Add the missing test helper**

`reviewReasons` exists on `rosterIngestHarness` (`internal/ingest/roster_test.go:566`) but not on `vsIngestHarness`, and the tests below need it. Both harnesses share `fakeIngestStore`, so add to `internal/ingest/vs_test.go`:

```go
// reviewReasons counts the queued review rows by reason, mirroring
// rosterIngestHarness's helper. The VS route now has several distinct reasons
// a row can be held for, and the assertions below care which gate a row hit
// rather than only that it was queued.
func (h *vsIngestHarness) reviewReasons() map[string]int {
	out := map[string]int{}
	for _, r := range h.store.Reviews {
		out[r.Reason]++
	}
	return out
}
```

- [ ] **Step 2: Write the failing tests**

Add to `internal/ingest/vs_test.go`:

```go
// A row nothing matches at AutoAccept, whose member is nonetheless the only
// candidate left once every other row is pinned. This is the whole gain: on
// capture 6 it is twelve members, including "Syłar" read as "cular" at 60.
func TestIngestVSResolvesAWeakRowFromTheResidual(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(2), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		// "Brav0z" scores below AutoAccept against Bravo02 and far below it
		// against Alpha01 -- unmatchable per-row, unambiguous once Alpha01 is
		// taken.
		{Text: "Brav0z", Confidence: 0.95}, {Text: "8,000,000", Confidence: 0.95},
	}

	res, err := h.IngestVS(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Matched != 2 {
		t.Errorf("matched %d, want 2: the residual row's member was the only one left", res.Matched)
	}
	if len(h.store.Facts) != 2 {
		t.Fatalf("wrote %d facts, want 2", len(h.store.Facts))
	}
	// The residual match must carry residualMatchConfidence, not score/100 --
	// a string score of ~60 blended into the fact would fall under
	// factConfidenceGate and the row would be queued anyway, silently undoing
	// the entire assignment.
	var residual db.Fact
	for _, f := range h.store.Facts {
		if f.MemberID == h.store.members[1].ID {
			residual = f
		}
	}
	if residual.Confidence < factConfidenceGate {
		t.Errorf("residual fact confidence %.2f is below factConfidenceGate %.2f; it would never be written",
			residual.Confidence, factConfidenceGate)
	}
	if residual.Confidence >= 0.95 {
		t.Errorf("residual fact confidence %.2f, want it visibly below a clean match", residual.Confidence)
	}
}

// The pinned self row appears twice by design (recon section 2). It is
// structurally expected, so it is counted and dropped -- never queued. A
// review row every week for something the screen always does trains an
// operator to ignore the queue.
func TestIngestVSCountsADuplicateRowRatherThanQueueingIt(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{
		captureComplete: true, rosterSize: 3, rankedRows: 3, duplicateSelfRow: true,
	})

	res, err := h.IngestVS(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Duplicates != 1 {
		t.Errorf("duplicates %d, want 1", res.Duplicates)
	}
	if reasons := h.reviewReasons(); reasons["no_confident_match"] != 0 {
		t.Errorf("the duplicate self row reached the review queue; all reasons: %v", reasons)
	}
	if res.Unidentified != 0 {
		t.Errorf("unidentified %d, want 0: a duplicate is not an unattributed row, and counting it as one would block every inferred zero",
			res.Unidentified)
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/ingest/ -run 'TestIngestVSResolvesAWeakRow|TestIngestVSCountsADuplicate' -v`
Expected: FAIL — `res.Duplicates undefined`, and the residual test matches only 1.

- [ ] **Step 4: Add the confidence constant and the result field**

In `internal/ingest/vs.go`, next to `zeroInferenceConfidence`:

```go
// residualMatchConfidence is the name-match confidence a phase-2 assignment
// carries.
//
// It exists because score/100 is the wrong confidence model for an
// assignment, and using it would silently erase the whole gain: a row
// resolved at string-score 60 blends to 0.60, falls under
// factConfidenceGate (0.80), and is queued for review despite having been
// resolved. Lowering that gate is not the fix -- it protects every other row.
//
// The claim a phase-2 assignment makes is not "these two strings are 60%
// similar". It is "this member is the unambiguous winner among the
// unclaimed, by a margin of at least ResidualMargin, in a closed set where
// every confident row is already pinned." That claim's strength does not vary
// with the string score, so neither does this number.
//
// 0.85 sits above factConfidenceGate and visibly below a clean match, so the
// distinction survives into the fact and a human triaging later can see how a
// row was resolved. UpsertFact only overwrites on strictly higher confidence,
// so a later clean read of the same member supersedes a residual match by
// itself -- which is the correct direction.
//
// Fitted, like confusableCost. Re-measure with `make probe-assign`; do not
// re-reason.
const residualMatchConfidence = 0.85
```

Extend `VSResult` (`internal/ingest/vs.go:265-268`):

```go
type VSResult struct {
	Matched, Queued, Zeroed, Unidentified int
	// Duplicates counts rows dropped because the member they read was
	// already assigned to another row -- the pinned self row, structurally.
	// Reported rather than silent because "the capture contained a duplicate"
	// and "the capture contained a row nobody could attribute" are different
	// outcomes, and only one of them is a problem.
	Duplicates int
	Status     string
}
```

- [ ] **Step 5: Wire the assignment into pass 2**

Add near `candidatesFor`:

```go
// scoreMatrix projects the rows' score vectors into the shape roster.Assign
// takes.
func scoreMatrix(rows []vsRow) [][]int {
	out := make([][]int, len(rows))
	for i, r := range rows {
		out[i] = r.Scores
	}
	return out
}
```

Replace the attribution loop from Task 2 Step 3 with:

```go
	assignments := roster.Assign(scoreMatrix(rows), roster.DefaultResidual)

	// A row left unassigned whose best read IS a member somebody else already
	// holds is a duplicate, not a failure -- see VSResult.Duplicates.
	assignedMember := make(map[int]bool, len(assignments))
	for _, a := range assignments {
		if a.Member >= 0 {
			assignedMember[a.Member] = true
		}
	}

	matchedRowCount := 0
	for n, row := range rows {
		a := assignments[n]
		dup := false
		if a.Member < 0 {
			for j, s := range row.Scores {
				if s >= roster.AutoAccept && assignedMember[j] {
					dup = true
					break
				}
			}
		}
		matched, err := run.attributeRow(ctx, i, row, a, dup, members)
		if err != nil {
			return VSResult{}, err
		}
		switch {
		case dup:
			run.res.Duplicates++
		case matched:
			matchedRowCount++
		default:
			run.res.Unidentified++
		}
	}
	_ = matchedRowCount
```

Then delete the identity cross-check block (`if matchedRowCount != len(scored) { ... }`, `vs.go:384-387`) together with the `_ = matchedRowCount` line and the `matchedRowCount` variable — the assignment makes one-member-per-row true by construction, so the check can no longer fire and a check that cannot fire is worse than none.

Delete `scored := map[int64]bool{}` as well; the assignment now owns that constraint.

- [ ] **Step 6: Rewrite `attributeRow` against the assignment**

Replace `attributeRow`'s signature and body:

```go
// attributeRow routes one already-read row to a fact, a duplicate no-op, or
// the review queue. It never creates a member: the roster route is the only
// writer of that table, so a row matching nothing goes to review, full stop.
//
// It no longer decides WHO the row is -- roster.Assign did that across the
// whole capture, using the constraint that a member appears at most once.
// What is left here is what to do about it.
func (run *vsRun) attributeRow(ctx context.Context, i *Ingester, row vsRow, a roster.Assignment, dup bool, members []roster.Member) (bool, error) {
	if dup {
		// Same member, a different screen position: the pinned self row.
		// Counted by the caller, deliberately not queued.
		return false, nil
	}
	if a.Member < 0 {
		candidates := candidatesFor(row, members)
		reason := "no_confident_match"
		if len(candidates) > 0 && candidates[0].Score >= roster.ReviewFloor {
			reason = "ambiguous_name_match"
		}
		return false, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.NameText, candidates, reason, 0)
	}

	memberID := members[a.Member].ID
	run.res.Matched++

	// A phase-1 match is a string that scored >= AutoAccept and is weighted
	// as one. A phase-2 match is an elimination argument whose strength does
	// not vary with the string score -- see residualMatchConfidence.
	matchNorm := float64(a.Score) / 100.0
	if a.Phase == roster.PhaseResidual {
		matchNorm = residualMatchConfidence
	}

	points, perr := ParsePoints(row.PointsText)
	if perr != nil {
		return true, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.PointsText, nil, "unparseable_points", 0)
	}
	conf := min(matchNorm, row.PointsConf)
	if conf < factConfidenceGate {
		return true, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.PointsText, nil, "low_confidence_points", conf)
	}
	if _, _, err := i.store.UpsertFact(ctx, db.Fact{
		MemberID: memberID, Metric: "vs_points", Value: float64(points),
		ObservedAt: run.observedAt, PeriodKey: run.periodKey,
		Source: "ocr:vs_ranking", ScreenshotID: row.ScreenshotID,
		Confidence: conf,
	}); err != nil {
		return true, fmt.Errorf("ingest: writing vs_points fact for member %d: %w", memberID, err)
	}
	return true, nil
}
```

Remove the now-unused `scored map[int64]bool` parameter everywhere and drop the `log/slog` import from `vs.go` if the cross-check was its only user (`go build` will say).

- [ ] **Step 7: Run the tests**

Run: `go test ./internal/ingest/ -v`
Expected: PASS. If `TestIngestVSDeduplicatesThePinnedSelfRow` fails on its warning expectation, update it — the cross-check it covered is gone and `TestIngestVSCountsADuplicateRowRatherThanQueueingIt` replaces it.

- [ ] **Step 8: Run the gate**

Run: `make gate-m4`
Expected: still FAIL (points work is Tasks 6-7), but with the name side moved. Record the exact line. The name bucket should drop sharply; expect roughly `matched=83` with `no_confident_match` + `ambiguous_name_match` down to about 3, and the row count somewhere in the mid-to-high 70s. **The points reasons will go UP** — every newly matched row brings a points read nobody had evaluated before, which the spec predicts explicitly.

- [ ] **Step 9: Commit**

```bash
git add internal/ingest/vs.go internal/ingest/vs_test.go
git commit -m "Attribute VS rows by assignment rather than per-row threshold

roster.Assign now decides who each row is, across the whole capture at once,
so attributeRow is left with what to do about it. The identity cross-check
goes with it: one-member-per-row is true by construction now, and a check that
cannot fire is worse than no check.

The confidence model is the part that does not fall out of the restructure.
score/100 against factConfidenceGate would queue an assignment-resolved row
anyway and silently undo the entire gain -- a row resolved at string-score 60
blends to 0.60 and never reaches a fact. The fix is not a lower gate, which
protects every other row: the claim a residual assignment makes is not 'these
strings are 60% similar' but 'this member is the unambiguous winner among the
unclaimed', and that claim's strength does not vary with the string score.

The pinned self row is now counted rather than queued. It is structurally
expected, and a review row every week for something the screen always does
trains an operator to ignore the queue. It is counted separately from
Unidentified on purpose -- folding it in would block every inferred zero,
since the zero rule requires a capture to have attributed every row."
```

---

## Task 4: Commit the assignment probe

**Files:**
- Create: `internal/ingest/zz_assign_probe_test.go` (promote the spike from the working tree)
- Modify: `internal/ingest/zz_name_probe_test.go` (the `probeFrame.OffsetPx` field, already added in the working tree)
- Modify: `Makefile`

**Interfaces:**
- Consumes: `loadProbeCapture`, `loadProbeFrames`, `probeMembers`, `parsePSMList`, `probeLoadedFrame`, `probeCapture` (all existing in `zz_name_probe_test.go`).
- Produces: `make probe-assign`.

The spike already exists in the working tree and is what produced every number in the spec. This task makes it durable. **Keep the canary and the decoy padding** — they are the only reason its zeros mean anything, and they are the first thing a future reader will want to re-run.

- [ ] **Step 1: Reconcile the spike against the shipped code**

The spike reimplemented the dedupe arithmetic inline. It cannot call `readVSRows` — that takes `[]db.CaptureFrame` and resolves each frame through the store, and the probe has decoded images and no database. What it *must* share is the dedupe decision, which is why Task 2 extracted `vsRowCursor`.

Replace the probe's `contentY`/`lastRowY` bookkeeping with `newVSRowCursor()`, `cursor.nextFrame(offsetBySeq[f.Seq])` and `cursor.accept(band, regionTop)`. The probe keeps its own frame walk and its own per-PSM loop; only the arithmetic that decides what counts as a row is shared. An instrument that reimplements that decision reports a row set production never sees.

Verify the file still carries, verbatim, the header comment explaining what the probe measures, the `decoyMembers` doc comment, and the canary's explanation.

- [ ] **Step 2: Add the Makefile target**

Append to `Makefile` after the `probe-points` target:

```make
# The M4 assignment probe: does closed-set matching beat per-row thresholding,
# and at what false-attribution cost?
#
# Not a gate. It asserts nothing and its output is the point. Reach for it
# before changing roster.ResidualFloor, roster.ResidualMargin or
# residualMatchConfidence -- and after, because "re-measure, do not re-reason"
# applies to all three.
#
# Two of its modes exist to keep its own numbers honest, and both should be run
# before believing a headline:
#
#   -probe.assignshuffle   rotates the truth labels by one rank, so every
#                          assignment is wrong by construction. It must report
#                          ~0 correct. A clean run here proved nothing until
#                          this fired, because forcing an assignment at floor 0
#                          / margin 0 produced a PERFECT result -- which reads
#                          as a finding and is really an untested instrument.
#   -probe.assigndecoys=N  pads the member set with N members one confusable
#                          substitution from a real name. The gate's capture is
#                          square (86 rows, 86 members) and production is not
#                          (recon: 94 ranked rows, 96 alliance members), and
#                          squareness is the assignment's biggest unearned
#                          advantage.
#
#	make probe-assign
#	make probe-assign PROBE_ARGS='-probe.assigndetail'
#	make probe-assign PROBE_ARGS='-probe.assignshuffle'
#	make probe-assign PROBE_ARGS='-probe.assigndecoys=20 -probe.assignpsm=13'
.PHONY: probe-assign
probe-assign: LW_BLOB_FS_ROOT ?= $(CURDIR)/data/blobs
probe-assign:
	LW_BLOB_FS_ROOT="$(LW_BLOB_FS_ROOT)" $(GO) test -tags m4probe -count=1 -v -timeout 60m ./internal/ingest/ -run TestM4AssignProbe -probe.assign $(PROBE_ARGS)
```

- [ ] **Step 3: Run all three modes**

```bash
make probe-assign
make probe-assign PROBE_ARGS='-probe.assignshuffle'
make probe-assign PROBE_ARGS='-probe.assigndecoys=20'
```

Expected, against capture 6: the plain run reports the baseline and the grid with the shipped floor/margin resolving ~83/86 at zero wrong; the shuffle run reports ~0 correct and ~71 wrong; the decoy run holds the gain with no *additional* wrong beyond the one phase 1 already produces.

- [ ] **Step 4: Confirm the probe stays out of `make test`**

Run: `go test ./...`
Expected: PASS, and the probe does not run — it is behind `//go:build m4probe`.

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/zz_assign_probe_test.go internal/ingest/zz_name_probe_test.go Makefile
git commit -m "Commit the assignment probe, canary and decoys included

The numbers that justified closed-set matching came from a throwaway harness.
That is exactly how vsNameOptions was set the first time, and rebuilding that
harness cost a session, so this one is committed.

Both of its self-checks are kept, because the headline is worthless without
them. A forced assignment at floor 0 / margin 0 produced a PERFECT result,
which reads as a finding and was really an untested instrument -- only
rotating the truth labels, and getting 0 correct against 71 wrong, established
that the counters measure attribution at all. And the gate's capture is square
where production is not, so the decoy padding is what keeps the squareness
from being read as a result.

It calls readVSRows rather than reimplementing the frame walk, so it cannot
drift from what IngestVS actually does."
```

---

## Task 5: Measure `ClosestPairScore` over the whole roster

**Files:**
- Modify: `internal/roster/confusable.go:106-126` (doc only)
- Modify: `internal/ingest/zz_name_probe_test.go:153-164`
- Modify: `internal/ingest/vs.go` (`IngestVS`, a warning)
- Test: `internal/ingest/vs_test.go`

**Interfaces:**
- Consumes: `roster.ClosestPairScore(names []string) (int, string, string)` (existing, unchanged signature).
- Produces: nothing new. This is a scoping fix.

Not an accuracy change. The decoy run exposed that today's shipped matcher can misattribute when the roster holds a near-neighbour of a ranked name, and `ClosestPairScore` does not catch it because the probe calls it over the 86 *ranked* names — a set that excludes exactly the members most likely to break it.

- [ ] **Step 1: Write the failing test**

Add to `internal/ingest/vs_test.go`:

```go
// Two members the matcher cannot tell apart are a hazard no threshold fixes
// and only an alias can, so ingest says so out loud rather than silently
// attributing one of them.
func TestIngestVSWarnsWhenTwoMembersAreIndistinguishable(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newVSHarness(t, "complete")
	// ALBAN80 and ALBANSO differ by one confusable substitution, which
	// confusable.go charges 2 tenths of an edit -- they score above
	// AutoAccept against each other.
	for _, name := range []string{"ALBAN80", "ALBANSO"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(1), 0)
	h.engine.Results = []ocr.Result{
		{Text: "ALBAN80", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if !strings.Contains(buf.String(), "indistinguishable") {
		t.Errorf("no warning logged for two members scoring above AutoAccept against each other; log was:\n%s", buf.String())
	}
}
```

Add `"bytes"`, `"log/slog"` and `"strings"` to `vs_test.go`'s imports if absent.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/ingest/ -run TestIngestVSWarnsWhenTwoMembers -v`
Expected: FAIL — no warning in the buffer.

- [ ] **Step 3: Implement the warning**

In `internal/ingest/vs.go`, immediately after `members := toRosterMembers(dbMembers, aliases)`:

```go
	// The safety half of confusable-aware scoring, checked against the roster
	// this run will actually match against. Two members scoring at or above
	// AutoAccept are a pair the matcher cannot tell apart, which no threshold
	// fixes and only an alias can -- and attributing one member's score to the
	// other is the single failure here a review queue cannot undo.
	//
	// Over the WHOLE member list, not the ranked rows. `make probe-m4`
	// measured it over the 86 transcribed names, which is the set least
	// likely to contain the problem: the ranking lists scorers only, so the
	// near-neighbour that breaks a match is disproportionately a member who
	// did not score and was therefore invisible to the check.
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	if closest, a, b := roster.ClosestPairScore(names); closest >= roster.AutoAccept {
		slog.WarnContext(ctx, "ingest: two roster members are indistinguishable to the matcher",
			"capture_id", captureID, "score", closest, "auto_accept", roster.AutoAccept,
			"member_a", a, "member_b", b)
	}
```

Re-add the `log/slog` import to `vs.go` if Task 3 removed it.

- [ ] **Step 4: Run the test**

Run: `go test ./internal/ingest/ -v`
Expected: PASS.

- [ ] **Step 5: Widen the probe's own check**

In `internal/ingest/zz_name_probe_test.go`, the `closest, a, b := roster.ClosestPairScore(names)` block builds `names` from `exp.Rows`. Change it to build from `probeMembers(exp)` and update the log line to say "among the N roster members" rather than "real names". Update `confusable.go`'s `ClosestPairScore` doc comment to state that it must be measured over the whole roster and why the ranked rows are the wrong set.

- [ ] **Step 6: Run the probe and the full suite**

```bash
make probe-m4
go test ./...
```
Expected: the probe still prints a closest-pair line with a margin below `AutoAccept`; `go test ./...` passes.

- [ ] **Step 7: Commit**

```bash
git add internal/roster/confusable.go internal/ingest/vs.go internal/ingest/vs_test.go internal/ingest/zz_name_probe_test.go
git commit -m "Measure the closest pair over the roster, not the ranking

ClosestPairScore is described in confusable.go as the guard that makes
cheapening substitutions safe to tune, and it was being measured over the 86
ranked names -- the set least likely to contain the problem. The weekly
ranking lists scorers only, so the near-neighbour that breaks a match is
disproportionately a member who did not score, and was therefore invisible to
the check meant to catch exactly that.

Found by padding the assignment probe's member set with decoys one confusable
substitution from a real name: today's shipped per-row matcher misattributes a
row under that padding, and the closest-pair reading stayed a comfortable 60
throughout.

Ingest now warns when any two members on the roster score at or above
AutoAccept against each other. No threshold fixes such a pair and only an
alias can, and it is worth knowing either way."
```

---

## Task 6: Points — monotonicity bounds

**Files:**
- Create: `internal/ingest/points.go`
- Test: `internal/ingest/points_test.go`
- Modify: `internal/ingest/vs.go` (pass 2 resolves points after the assignment)

**Interfaces:**
- Consumes: `ParsePoints(s string) (int64, error)`, `ErrUnparseable`, `attributeRow` (Task 3).
- Produces: `attributeRow` reaches its final signature here — `func (run *vsRun) attributeRow(ctx context.Context, i *Ingester, row vsRow, a roster.Assignment, dup bool, bound pointsBound, members []roster.Member) (bool, error)`. Also:
  ```go
  type pointsBound struct{ Lo, Hi int64 }
  func pointsBounds(values []int64, known []bool) []pointsBound
  const pointsUnknown = int64(-1)
  ```
  `values[i]` is row *i*'s parsed points (any value when `known[i]` is false); the result is one inclusive bound per row derived from the nearest known neighbours above and below. `Lo` is 0 and `Hi` is `math.MaxInt64` where no neighbour constrains that side.

The ranking is sorted descending, so once the assignment fixes each row's rank, row *i*'s points are bounded by its nearest confidently-parsed neighbours. That is a structural check of the same species as roster ingest's "the group header states its own total".

- [ ] **Step 1: Write the failing tests**

Create `internal/ingest/points_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/ingest/ -run TestPointsBounds -v`
Expected: FAIL — `undefined: pointsBounds`

- [ ] **Step 3: Implement**

Create `internal/ingest/points.go`:

```go
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

// pointsBounds returns one bound per row. values[i] is meaningful only where
// known[i]; unknown rows are bracketed by the nearest KNOWN neighbour on each
// side, looking past other unknowns rather than resting on a value that was
// never read. An end with no neighbour is left open (0 or math.MaxInt64)
// rather than pinned to something that does not exist.
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
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/ingest/ -run TestPointsBounds -v`
Expected: PASS

- [ ] **Step 5: Commit the pure part**

```bash
git add internal/ingest/points.go internal/ingest/points_test.go
git commit -m "Bound each VS row's points by its neighbours' points

The weekly ranking is sorted descending, so once the assignment fixes each
row's rank a row's value is bracketed by the nearest confidently-parsed rows
above and below. A structural check of the same species as roster ingest's
'the group header states its own total' -- one where the alternative is a
tuned number.

Unknown rows look PAST other unknowns to the nearest known neighbour, rather
than resting a bound on a value that was never read, and the ends are left
open rather than pinned to a neighbour that does not exist."
```

- [ ] **Step 6: Write the failing integration test for reject-and-corroborate**

Add to `internal/ingest/vs_test.go`:

```go
// A value that parses and sits in order is corroborated by the ordering, so
// it no longer rests on OCR confidence alone. Measured on capture 6: two of
// the six points failures were EXACTLY RIGHT and rejected for confidence --
// 8,835,180 at 0.52 and 1,242,375 at 0.70.
func TestIngestVSAcceptsALowConfidencePointsReadThatSitsInOrder(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		{Text: "Bravo02", Confidence: 0.95}, {Text: "8,000,000", Confidence: 0.52},
		{Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	res, err := h.IngestVS(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if len(h.store.Facts) != 3 {
		t.Errorf("wrote %d facts, want 3: 8,000,000 sits between its neighbours and is corroborated by the ordering", len(h.store.Facts))
	}
	if reasons := h.reviewReasons(); reasons["low_confidence_points"] != 0 {
		t.Errorf("an in-order value was rejected on OCR confidence alone; all reasons: %v", reasons)
	}
	_ = res
}

// The other half, and the reason the first half is safe: a value that parses
// but violates its bounds is the signature of a crop that caught neighbouring
// content, which is exactly what vsPointsSpec's charset comment describes.
func TestIngestVSRejectsAPointsValueThatBreaksTheRankingOrder(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		// Rank 2 cannot outscore rank 1.
		{Text: "Bravo02", Confidence: 0.95}, {Text: "99,000,000", Confidence: 0.95},
		{Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if reasons := h.reviewReasons(); reasons["points_out_of_order"] == 0 {
		t.Errorf("a value above its own better-ranked neighbour was written as a fact; all reasons: %v", reasons)
	}
}
```

- [ ] **Step 7: Run to verify they fail**

Run: `go test ./internal/ingest/ -run 'TestIngestVSAcceptsALowConfidence|TestIngestVSRejectsAPointsValue' -v`
Expected: FAIL — the first writes 2 facts, the second queues nothing with that reason.

- [ ] **Step 8: Resolve points after the assignment**

`attributeRow` currently decides points per row in isolation, which cannot see the ordering. Split it: the loop in `IngestVS` first resolves every assigned row's points, then writes.

In `internal/ingest/vs.go`, replace the attribution loop body's points handling by computing a parsed-value array before the write loop:

```go
	// Points resolve against the ranking's own order, which needs every row's
	// value in hand -- so parse first, bound second, write third. Rows are in
	// rank order because they are in screen order.
	values := make([]int64, len(rows))
	known := make([]bool, len(rows))
	for n, row := range rows {
		if assignments[n].Member < 0 {
			continue
		}
		v, err := ParsePoints(row.PointsText)
		if err != nil {
			continue
		}
		// Only a confident read seeds a bound. A weak read is what the bounds
		// exist to adjudicate, and letting it define the window it is judged
		// against would make the check circular.
		if row.PointsConf >= factConfidenceGate {
			values[n], known[n] = v, true
		}
	}
	bounds := pointsBounds(values, known)
```

Pass `bounds[n]` into `attributeRow` (new parameter `bound pointsBound`) and replace its confidence branch:

```go
	points, perr := ParsePoints(row.PointsText)
	if perr != nil {
		return true, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.PointsText, nil, "unparseable_points", 0)
	}
	if !withinBounds(points, bound) {
		// A value outside the window its neighbours define is the signature
		// of a crop that caught neighbouring content -- the failure
		// vsPointsSpec's charset comment describes, caught structurally.
		return true, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.PointsText, nil, "points_out_of_order", 0)
	}
	// Everything past here is in order, so a weak read is no longer resting
	// on OCR confidence alone and clears the write gate on the ordering's
	// evidence instead. It is raised to exactly factConfidenceGate and no
	// further: invariant #5 is about what a number claims about itself, and
	// being in order does not make a 0.52 read a 0.95 one -- it only makes it
	// worth writing.
	conf := min(matchNorm, row.PointsConf)
	if conf < factConfidenceGate {
		conf = factConfidenceGate
	}
```

The `low_confidence_points` branch disappears entirely from the VS route: a value is now either out of order (queued) or in order (written). Leave the reason string in place elsewhere — `IngestRoster` still uses its own equivalents, and the review UI reads historical rows.

- [ ] **Step 9: Run the tests**

Run: `go test ./internal/ingest/ -v`
Expected: PASS.

- [ ] **Step 10: Run the gate**

Run: `make gate-m4`
Expected: another jump. Record the line.

- [ ] **Step 11: Commit**

```bash
git add internal/ingest/vs.go internal/ingest/vs_test.go
git commit -m "Judge VS points against the ranking's own order

Two uses of one structural fact. A value outside the window its neighbours
define is rejected -- that is the signature of a crop that caught neighbouring
content, which is the failure vsPointsSpec's charset comment describes, now
caught structurally rather than hoped away. And a value INSIDE that window no
longer rests on OCR confidence alone, which is what recovers the two reads
capture 6 got exactly right and threw away for confidence: 8,835,180 at 0.52
and 1,242,375 at 0.70.

Only a confident read seeds a bound. Letting a weak read define the window it
is then judged against would make the check circular.

The fact still carries the read's own confidence. Invariant #5 is about what a
number claims about itself, and being in order does not make a 0.52 read a
0.95 one -- it only makes it worth writing."
```

---

## Task 7: Points — bounded retry on an empty read

**Files:**
- Modify: `internal/ingest/vs.go` (`vsPointsRetry`, `readVSRows`)
- Test: `internal/ingest/vs_test.go`

**Interfaces:**
- Consumes: `readFieldWithRetry`, `readPlan`, `ocr.PSMRawLine`, `pointsBounds`, `withinBounds`.
- Produces: `var vsPointsRetry readPlan`; `vsRow` gains `PointsRetryText string` and `PointsRetryConf float64`.

Two of capture 6's six points failures are empty reads — the same layout blindness the name field already retries for. The retry was withheld from a numeric field because it can manufacture a plausible value. Bounds are the guard that answers that: **the retry's value is accepted only if it parses AND satisfies its bounds.**

- [ ] **Step 1: Write the failing test**

Add to `internal/ingest/vs_test.go`:

```go
// The empty points read is the same PSM 7 layout blindness the name field
// retries for. It is allowed here only because the value has to land inside
// the window its neighbours define -- a manufactured number has no reason to.
func TestIngestVSRetriesAnEmptyPointsReadAndAcceptsAnInOrderValue(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		// Empty at the primary PSM, read at the retry.
		{Text: "Bravo02", Confidence: 0.95}, {Text: "", Confidence: 0},
		{Text: "8,000,000", Confidence: 0.90},
		{Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if len(h.store.Facts) != 3 {
		t.Errorf("wrote %d facts, want 3: the retry read a value that sits between its neighbours", len(h.store.Facts))
	}
}

// The guard that makes the retry defensible at all. A raw-line retry on a crop
// that caught neighbouring content can produce a well-formed number, and
// nothing about the number itself says so -- only its position does.
func TestIngestVSRejectsARetriedPointsValueThatBreaksTheOrder(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		{Text: "Bravo02", Confidence: 0.95}, {Text: "", Confidence: 0},
		{Text: "44,357,000", Confidence: 0.90}, // well-formed, and impossible at rank 2
		{Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if reasons := h.reviewReasons(); reasons["points_out_of_order"] == 0 {
		t.Errorf("a fabricated but well-formed retry value was written; all reasons: %v", reasons)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/ingest/ -run 'TestIngestVSRetriesAnEmpty|TestIngestVSRejectsARetried' -v`
Expected: FAIL — no retry happens, so the fake engine's queue desynchronises and both fail.

- [ ] **Step 3: Add the retry plan**

In `internal/ingest/vs.go`, beside `vsName`/`vsNameRetry`:

```go
// vsPointsRetry reads a points crop the primary PSM returned nothing for.
//
// The asymmetry with the name field was deliberate and is now conditional
// rather than absolute. A name has a known roster behind it, so a bad read
// simply fails to match; a number has no such guard, and a raw-line retry
// could manufacture a plausible value from a crop that caught neighbouring
// content. That objection is answered by the bounds in points.go and not
// otherwise: a retried value is written only when it parses AND lands inside
// the window its neighbours define, which a fabricated number has no reason
// to do.
//
// It keeps vsPointsOptions rather than borrowing vsNameRetry's grayscale-plus-
// invert at upscale 4. Those were fitted for the name crop; measure this one
// with `make probe-points` before changing it.
var vsPointsRetry = readPlan{
	spec: ocr.Spec{MinConf: vsPointsSpec.MinConf, PSM: ocr.PSMRawLine},
	opts: vsPointsOptions,
}
```

- [ ] **Step 4: Read the retry in pass 1**

In `readVSRows`, replace the points read with:

```go
			pointsRes, err := i.readFieldWithRetry(ctx, img,
				fieldRect(band, img, vsPointsXFrac0, vsPointsXFrac1, vsPointsYFrac0, vsPointsYFrac1),
				readPlan{spec: vsPointsSpec, opts: vsPointsOptions}, vsPointsRetry)
			if err != nil {
				return nil, err
			}
```

`readFieldWithRetry` already fires on the empty string only, which is exactly the condition wanted, so no new mechanism is needed — the bounds in Task 6 do the guarding at write time.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/ingest/ -v`
Expected: PASS.

- [ ] **Step 6: Measure the retry's preprocessing**

Run: `make probe-points`
Read the output. If the shipped `vsPointsOptions` are not the best setting for the retry, record the measured table in `vsPointsRetry`'s doc comment and change the options to match — the name-side equivalent was worth two members, and inheriting is an assumption, not a result.

- [ ] **Step 7: Run the gate and commit**

```bash
make gate-m4
git add internal/ingest/vs.go internal/ingest/vs_test.go
git commit -m "Retry an empty points read, guarded by the ranking's order

Two of capture 6's six points failures are empty reads -- the same PSM 7
layout blindness the name field has retried for since the raw-line fix. The
retry was withheld here because a number has no roster behind it and a
raw-line pass over a crop that caught neighbouring content can manufacture a
well-formed value.

That objection is answered by the bounds and not otherwise: a retried value is
written only when it parses AND lands inside the window its neighbours define,
which a fabricated number has no reason to do. So the asymmetry between the
two fields becomes conditional rather than absolute, and the condition is
structural rather than tuned.

readFieldWithRetry already fires on the empty string alone, so this needed no
new mechanism -- only a plan for the field and a guard at write time."
```

---

## Task 8: PSM 7+13 union on the name field

**Files:**
- Modify: `internal/ingest/vs.go` (`readVSRows`)
- Test: `internal/ingest/vs_test.go`

**Interfaces:**
- Consumes: `readFieldWithRetry`, `roster.Rank`, `ocr.PSMRawLine`.
- Produces: `var vsNameModes []readPlan`.

Worth +6 at the pre-assignment baseline and +1 once Task 3 is in. It is kept because it is **insurance rather than accuracy**: it matters on a capture where the assignment has less structure to work with, which is exactly the case no single fixture can measure.

This must be justified against CLAUDE.md's standing objection that gating a retry on a low *match* score "would put the matcher upstream of OCR." It does not apply: both reads are produced unconditionally, so nothing about the roster decides what OCR is asked to do.

- [ ] **Step 1: Write the failing test**

Add to `internal/ingest/vs_test.go`:

```go
// Both segmentation modes run on every name crop and the better score wins.
// Their miss sets are disjoint in four places on capture 6, so this is not a
// tie-break, it is two independent readings of the same pixels.
func TestIngestVSTakesTheBetterOfTwoNameReads(t *testing.T) {
	h := newVSHarness(t, "complete")
	h.store.nextMemberID++
	h.store.members = append(h.store.members, db.Member{
		ID: h.store.nextMemberID, AllianceID: 1, Name: "Alpha01",
		NameNormalized: roster.Normalize("Alpha01"), Active: true,
	})
	h.addFrame(vsFrame(1), 0)
	h.engine.Results = []ocr.Result{
		{Text: "XXXXXXX", Confidence: 0.95}, // primary mode: matches nobody
		{Text: "Alpha01", Confidence: 0.95}, // second mode: exact
		{Text: "9,000,000", Confidence: 0.95},
	}

	res, err := h.IngestVS(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Matched != 1 {
		t.Errorf("matched %d, want 1: the second mode read the name exactly", res.Matched)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/ingest/ -run TestIngestVSTakesTheBetterOfTwo -v`
Expected: FAIL — only one name read happens, so the row matches nobody.

- [ ] **Step 3: Implement**

In `internal/ingest/vs.go`, beside `vsName`:

```go
// vsNameModes are the segmentation modes every name crop is read at. Each
// produces its own read; the per-member best score across all of them is what
// the assignment sees.
//
// PSM 13 alone is much worse than PSM 7 alone -- 55/86 members against 73/86
// -- but their miss sets are DISJOINT in four places on capture 6, so the
// union beats either. Worth +6 at the per-row baseline and +1 once closed-set
// assignment is in, which makes this insurance rather than accuracy: it earns
// its keep on a capture where the assignment has less structure to work with,
// which is exactly the case a single fixture cannot measure.
//
// This is not the thing CLAUDE.md warns against when it says gating a retry on
// a low match score "would put the matcher upstream of OCR". Both reads happen
// unconditionally, so nothing about the roster decides what OCR is asked to
// do; taking the better of two independent readings of the same pixels is the
// move the probes already make across overlapping frames.
//
// Cost is one extra tesseract invocation per row, roughly 27s to 53s over 86
// rows, in a batch that runs once a day.
var vsNameModes = []readPlan{
	vsName,
	{spec: ocr.Spec{MinConf: vsNameSpec.MinConf, PSM: ocr.PSMRawLine, Languages: vsNameLanguages}, opts: vsNameOptions},
}
```

In `readVSRows`, replace the single name read and the score-vector build with:

```go
			scores := make([]int, len(members))
			nameText := ""
			bestTop := -1
			for _, mode := range vsNameModes {
				nameRes, err := i.readFieldWithRetry(ctx, img,
					fieldRect(band, img, vsNameXFrac0, vsNameXFrac1, vsNameYFrac0, vsNameYFrac1),
					mode, vsNameRetry)
				if err != nil {
					return nil, err
				}
				if nameRes.Text == "" {
					continue
				}
				top := 0
				for _, c := range roster.Rank(nameRes.Text, members) {
					if j := idxByID[c.MemberID]; c.Score > scores[j] {
						scores[j] = c.Score
					}
					if c.Score > top {
						top = c.Score
					}
				}
				// NameText is only ever shown to a human in the review queue,
				// so it carries whichever read got closest to a real member --
				// the one worth looking at when deciding what the row says.
				if top > bestTop {
					bestTop, nameText = top, nameRes.Text
				}
			}
```

and set `NameText: nameText` in the `vsRow` literal.

- [ ] **Step 4: Update the fixtures for the extra read**

Every VS fixture queues one name result per row. Two modes means two. Update `newVSIngestHarness` in `internal/ingest/vs_test.go` to emit each row's name twice:

```go
	for _, r := range rows {
		results = append(results,
			ocr.Result{Text: r.name, Confidence: 0.95},
			ocr.Result{Text: r.name, Confidence: 0.95},
			ocr.Result{Text: r.points, Confidence: 0.95},
		)
	}
```

Do the same for every hand-built `h.engine.Results` in the tests added by Tasks 3, 5, 6 and 7.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/ingest/ -v`
Expected: PASS.

- [ ] **Step 6: Run the gate and the probes**

```bash
make gate-m4
make probe-assign
```

- [ ] **Step 7: Commit**

```bash
git add internal/ingest/vs.go internal/ingest/vs_test.go
git commit -m "Read every name crop at both segmentation modes

PSM 13 alone is much worse than PSM 7 alone -- 55/86 members against 73/86 --
but their miss sets are disjoint in four places on capture 6, so the union
beats either. The per-member best score across both reads is what the
assignment sees.

This is not the move CLAUDE.md warns against when it says a retry gated on a
low match score would put the matcher upstream of OCR. Both reads happen
unconditionally, so nothing about the roster decides what OCR is asked to do.

Worth six members at the per-row baseline and one once assignment is in, which
makes it insurance rather than accuracy: it earns its keep on a capture where
the assignment has less structure to work with, and that is exactly the case a
single committed fixture cannot measure."
```

---

## Task 9: Run the gate, record the result, update the docs

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/superpowers/specs/2026-08-17-m4-closed-set-matching-design.md` (a results section)
- Modify: `docs/superpowers/specs/2026-08-17-m4-gate-name-matching-gap.md` (mark superseded)

- [ ] **Step 1: Run everything**

```bash
go test ./...
make verify-nocgo
make gate-m4
make probe-assign
make probe-assign PROBE_ARGS='-probe.assignshuffle'
make probe-assign PROBE_ARGS='-probe.assigndecoys=20'
make probe-m4
make probe-points
```

Record each headline number verbatim. **Do not paraphrase a number you did not read.**

- [ ] **Step 2: Decide whether Task 10 is needed**

If `make gate-m4` reports ≥82/86, the gate passes and **Task 10 is dropped** — the spec says so explicitly. Record the decision and the number that drove it.

If it is short, read the remaining failures by reason before starting Task 10: the two symbol-for-digit rows are the only failures Task 10 addresses, and if the shortfall is elsewhere, Task 10 is not the fix.

- [ ] **Step 3: Add a results section to the design doc**

Append a `## 9. Results` section with the before/after gate lines, the probe grid at the shipped floor and margin, the shuffle and decoy readings, and the final review-queue reason breakdown. State the game version and the capture.

- [ ] **Step 4: Update `CLAUDE.md`**

Add to the testing section:

```
- `make probe-assign` — **not a gate**: the measuring instrument for closed-set
  matching. Reach for it before changing `roster.ResidualFloor`,
  `ResidualMargin` or `residualMatchConfidence`, and after. Two of its modes
  keep its own numbers honest and both should be run before believing a
  headline: `-probe.assignshuffle` rotates the truth labels so every
  assignment is wrong by construction (it must report ~0 correct), and
  `-probe.assigndecoys=N` pads the member set, because the gate's capture is
  square and production is not.
```

Add a subsection recording the two durable lessons:

```
### A clean measurement is not a validated one

The assignment probe reported zero misattributions at every setting, including
a forced assignment at floor 0 / margin 0 — which produced a *perfect* result.
That reads as an overwhelming finding and was, at that point, an untested
instrument: nothing had shown the probe could report a wrong assignment at all.
Rotating the truth labels by one rank did, at 0 correct against 71 wrong.

This is the "broken instrument reports agreement" lesson with the sign
flipped. There, implausible *uniformity* was the tell. Here the result was
plausible and simply unvalidated, and the discipline is the same either way:
before believing a measurement, establish that it can produce the answer you
are hoping not to see.

### The gate's capture is square and production is not

`fixtures/m4gate/expected.yaml` transcribes only the ranked rows, so a member
list built from it gives every member a row. The weekly ranking lists scorers
only — recon measured 94 ranked rows against 96 alliance members — so the
residual pool in production always holds members no row can claim. Any
measurement that keys on the closed-set structure must be re-run with
`-probe.assigndecoys` before it means anything about a real capture.
```

- [ ] **Step 5: Mark the predecessor superseded**

Add to the top of `2026-08-17-m4-gate-name-matching-gap.md`:

```
**Superseded** by `2026-08-17-m4-closed-set-matching-design.md` for everything
under "What is left". Its account of how the name field reached 71/86 stands,
and so does its lesson about two aggregates read as one causal claim.
```

- [ ] **Step 6: Commit**

```bash
git add CLAUDE.md docs/superpowers/specs/
git commit -m "Record the closed-set matching results

Adds probe-assign to the testing section with the two self-check modes that
have to be run before its headline means anything, and writes down the two
lessons worth keeping.

The first is the 'broken instrument reports agreement' rule with the sign
flipped: the assignment probe's zeros were plausible rather than implausible,
and were still unvalidated, because nothing had shown it could report a wrong
assignment at all. A forced assignment at floor 0 / margin 0 producing a
perfect result should have been the tell and instead read as a triumph.

The second is that the gate's capture is square where production is not, so
any measurement keying on the closed-set structure has to be re-run with
decoy padding before it says anything about a real capture."
```

---

## Task 10 (conditional): Repair a symbol-for-digit points read

**Run this task only if Task 9 Step 2 showed the gate short AND the shortfall includes the symbol-for-digit rows.** The spec lists it last because it is the only item that constructs a value rather than validating one.

**Files:**
- Modify: `internal/ingest/points.go`
- Test: `internal/ingest/points_test.go`

**Interfaces:**
- Consumes: `pointsBound`, `withinBounds`, `ParsePoints`.
- Produces: `func repairPoints(raw string, b pointsBound) (int64, bool)` — returns the repaired value and whether the bounds admitted exactly one.

Capture 6's cases: `¢,609,299` (albambet, want 2,609,299) and `e,2¢8,001` (ZeL1, want 2,328,001). The comma grouping is intact in both; a single non-digit occupies a digit position.

- [ ] **Step 1: Write the failing tests**

Add to `internal/ingest/points_test.go`:

```go
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
	if got, ok := repairPoints("¢6092 99", pointsBound{Lo: 2_600_000, Hi: 2_613_585}); ok {
		t.Errorf("repairPoints = %d, true; want refusal on broken grouping", got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/ingest/ -run TestRepairPoints -v`
Expected: FAIL — `undefined: repairPoints`

- [ ] **Step 3: Implement**

Add to `internal/ingest/points.go`:

```go
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
```

Add `"strings"` and `"unicode/utf8"` to `points.go`'s imports.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/ingest/ -run TestRepairPoints -v`
Expected: PASS

- [ ] **Step 5: Wire it into `attributeRow`**

Where `ParsePoints` fails, try `repairPoints(row.PointsText, bound)` before queueing `unparseable_points`. A repaired value takes `min(matchNorm, row.PointsConf)` as its confidence like any other read.

- [ ] **Step 6: Run the gate**

Run: `make gate-m4`

- [ ] **Step 7: Commit**

```bash
git add internal/ingest/points.go internal/ingest/points_test.go internal/ingest/vs.go
git commit -m "Solve for a single symbol-for-digit points misread

Capture 6 has two reads whose comma grouping is intact and which lose exactly
one digit to a symbol: '¢,609,299' against a true 2,609,299 and 'e,2¢8,001'
against 2,328,001.

This is the only place in the pipeline that constructs a value rather than
validating one, so it is fenced hard: comma grouping intact, exactly one
damaged position, and the bounds must admit exactly ONE digit there. Two
admissible digits is not a near miss, it is a guess, and a guessed number on a
leaderboard is the failure a review queue cannot undo.

The repaired value still points at the same crop of the same screenshot and
still carries the read's own confidence, so it remains an observation about
pixels -- narrowed by an ordering the same capture establishes."
```

---

## Notes for the executor

- **The gate is the acceptance test and it is slow** (~40s). Run it at the end of Tasks 2, 3, 6, 7, 8 and 9 as instructed, not after every step.
- **`make gate-m4` needs Docker up** (`docker compose up -d`) and tesseract on PATH, and reads the blob store — the Makefile defaults `LW_BLOB_FS_ROOT` to an absolute path with `?=` for the reason CLAUDE.md's gotchas explain.
- **Tasks 1 and 6 Steps 1-5 need nothing running.** They are pure.
- **If a task's gate number moves the wrong way, stop.** Do not proceed to the next task hoping it nets out. Each task's expected effect is stated; a surprise is information.
- **`fixtures/m4gate/expected.yaml` is never edited.** If a test seems to need it changed, the test is wrong.
