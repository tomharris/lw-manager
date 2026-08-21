# M4 Roster Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the roster route a hand-transcribed ground truth and a gate that can go red, then fix the defects that gate exposes until ≥95% of the alliance's members are created with zero splits.

**Architecture:** A new fixture (`fixtures/m4rostergate/expected.yaml`) transcribed off capture 1's frames, a new build-tagged gate (`make gate-roster`) asserting four conditions against it, and new probe modes for the three fields that have never been measured. Fixes are applied only after a red baseline is recorded, and each is measured through a probe before and after — never reasoned from OCR output.

**Tech Stack:** Go (`CGO_ENABLED=0`), Postgres via `internal/dbtest`, tesseract CLI subprocess, `go.yaml.in/yaml/v3`, hand-rolled NCC in `internal/vision`.

**Spec:** `docs/superpowers/specs/2026-08-20-m4-roster-gate-design.md`

## Global Constraints

- `CGO_ENABLED=0` always. No gocv, gosseract, or onnxruntime_go. OCR is the `tesseract` CLI as a subprocess.
- **No absolute pixel coordinates outside a `Transport` implementation.** Everything upstream speaks `transport.Norm` / `transport.Rect` in `[0,1]`.
- **Facts are append-only.** Corrections supersede via `superseded_by`; nothing is mutated in place.
- **All vision logic ships with fixture-based tests that run with no device attached.** `go test ./...` must pass with no emulator, no adb, no Docker.
- `context.Context` is the first parameter of anything doing I/O. Errors wrap with `%w` and enough context to locate the failure.
- All logging through `log/slog` to **stderr**; CLI results to stdout.
- Sentinel errors compared with `errors.Is`/`errors.As`, never by string.
- **Commit messages carry no attribution to Claude or Claude Code.** Imperative mood, and they explain *why* — match the voice of `git log`.
- **Measure, do not re-reason.** Any change to a crop, a preprocessing option, a PSM or a matcher constant is read against a probe before and after. A crop "verified by eye" is not measured.
- The test database is `lw_manager_test` via `internal/dbtest`, which reads `LW_TEST_DATABASE_URL` and never `LW_DATABASE_URL`.
- Gates reading the blob store need an **absolute** `LW_BLOB_FS_ROOT`; Makefile targets default it with `?=`.

## File Structure

| file | responsibility |
|---|---|
| `internal/ingest/vs.go` | modify: guard zero-inference on rows actually parsed |
| `internal/ingest/vs_test.go` | modify: the vacuous-guard test |
| `fixtures/m4rostergate/expected.yaml` | create: the hand-transcribed ground truth |
| `fixtures/m4rostergate/README.md` | create: how it was produced and what the gate asserts |
| `internal/ingest/roster_gate_test.go` | create: the gate, tag `m4rostergate` |
| `internal/ingest/roster.go` | modify: expose `GroupTally.MatchedOrCreated`; header crop constants |
| `internal/ingest/parse_test.go` | modify: real chevron-bleed strings as fixtures |
| `internal/ingest/zz_roster_probe_test.go` | modify: header / power / level probe modes; repoint truth at `expected.yaml` |
| `Makefile` | modify: `gate-roster` target; probe docs |
| `CLAUDE.md`, `README.md` | modify: the roster gate, the tolerance ruling, retiring the lower-bound caveat |

---

### Task 1: Guard zero-inference on a capture that actually parsed rows

`internal/ingest/vs.go:753` reads `capture.Status == "complete" && run.res.Unidentified == 0`. On a capture that parsed **no rows at all**, `Unidentified == 0` holds vacuously, so every member on the roster is written an inferred zero at confidence 0.90 — a confident number on a leaderboard derived from no read whatsoever. It also poisons the correction path: `UpsertFact` only overwrites on strictly higher confidence, so those zeros outrank the real reads a later ingest produces.

**Files:**
- Modify: `internal/ingest/vs.go:753`
- Test: `internal/ingest/vs_test.go`

**Interfaces:**
- Consumes: `vsFixture{captureComplete, rosterSize, rankedRows, ghostRows, duplicateSelfRow}`, `newVSIngestHarness(t, fx)`, `VSResult{Matched, Queued, Zeroed, Unidentified, Duplicates, Status}` — all already in `vs_test.go` and `vs.go`.
- Produces: nothing new. This task changes one boolean expression.

- [ ] **Step 1: Write the failing test**

Add to `internal/ingest/vs_test.go`, directly below `TestIngestVSInfersNoZeroesWhileAnyRowIsUnidentified` so the two conditions read together:

```go
// A capture that parsed no rows has not proved anyone absent either, and the
// Unidentified == 0 half of the zero-inference guard is no protection here:
// with nothing parsed, nothing could fail to be identified, so the condition
// holds VACUOUSLY and every member on the roster is zeroed at 0.90 on the
// strength of no read at all. That is the same 0.90-on-a-failed-read defect
// the test above exists for, reached from the other side, and it is worse in
// one respect: UpsertFact only overwrites on strictly higher confidence, so
// these zeroes outrank the real values a later ingest produces.
//
// A capture can reach ingest in this state — status is set at capture time by
// the route that proved it reached the list bottom, and a route can prove that
// on a screen whose rows then fail to segment.
func TestIngestVSInfersNoZeroesWhenNothingWasParsed(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{
		captureComplete: true,
		rosterSize:      3,
		rankedRows:      0,
	})

	res, err := h.IngestVS(context.Background(), 1, "2026-W33")
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Zeroed != 0 {
		t.Errorf("zeroed %d members from a capture that parsed no rows, want 0", res.Zeroed)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails, and record how**

```bash
cd /home/tom/Projects/lw-manager
go test ./internal/ingest/ -run TestIngestVSInfersNoZeroesWhenNothingWasParsed -v
```

Expected: FAIL with `zeroed 3 members from a capture that parsed no rows, want 0`.

**If instead it errors before reaching the guard** — `SegmentRows` returning an error rather than `nil, nil` on a frame with no cards — the fixture cannot express "zero rows" and you must construct the capture differently: keep `rankedRows: 1` and add a `noRows bool` to `vsFixture` that appends no frame at all. Say which happened in the commit message; do not paper over it.

- [ ] **Step 3: Make the fix**

In `internal/ingest/vs.go`, `totalParsed` is already in scope at line 626 (`totalParsed := len(rows)`). Change the guard:

```go
	if capture.Status == "complete" && run.res.Unidentified == 0 && totalParsed > 0 {
```

And extend the comment block directly above it, after the paragraph ending "…once every row is accounted for.":

```go
	// The third condition closes a hole the first two leave open. Both of
	// them are about rows that WERE parsed — one about the capture's own
	// completeness claim, one about rows held in review — and neither says
	// anything when there are no rows at all. With none, Unidentified == 0
	// holds vacuously and this loop zeroes the entire roster on the strength
	// of no read whatsoever, which invariant #5 forbids in its plainest form.
	// A capture reaches that state whenever the route proved it reached the
	// list bottom on a screen whose rows then failed to segment.
```

- [ ] **Step 4: Run the test and the full ingest suite**

```bash
go test ./internal/ingest/ -run TestIngestVSInfersNoZeroesWhenNothingWasParsed -v
go test ./internal/ingest/
```

Expected: both PASS. The other zero-inference tests (`TestIngestVSWritesZeroesOnlyForACompleteCapture`, `…InfersZeroesOnceEveryRowIsIdentified`, `…InfersZeroOnTheRightMemberWhenRowsArriveOutOfRosterOrder`) must be unaffected — they all parse rows.

- [ ] **Step 5: Mutation-check the guard**

Temporarily revert only the `&& totalParsed > 0` clause and re-run the new test. It must go red. Restore the clause. This is the step that proves the test pins the fix rather than passing for an unrelated reason — the repo has been bitten by a test that pinned a check's existence but not its value.

```bash
go test ./internal/ingest/ -run TestIngestVSInfersNoZeroes -v
```

- [ ] **Step 6: Commit**

```bash
git add internal/ingest/vs.go internal/ingest/vs_test.go
git commit -m "Refuse to infer zeroes from a capture that parsed no rows

The zero-inference guard read `complete && Unidentified == 0`. With no rows
parsed at all, nothing could fail to be identified, so the second half held
VACUOUSLY and every member of the alliance was written an inferred zero at
confidence 0.90 on the strength of no read whatsoever.

Worse than a wrong number, because UpsertFact only overwrites on strictly
higher confidence: those zeroes then outrank the real values a later ingest
produces, so the capture that finally reads correctly changes nothing.

Recorded as I6 in the closed-set branch's final review and verified present on
main, neither introduced nor worsened there. The test is mutation-checked --
removing the new clause turns it red."
```

---

### Task 2: The fixture schema, its loader, and its shape checks

Before ~96 members are transcribed, the format they go into must be settled and machine-checked. This task builds the loader and its guards against a deliberately tiny fixture, so a format mistake costs three rows rather than ninety-six.

**Files:**
- Create: `fixtures/m4rostergate/README.md`
- Create: `internal/ingest/roster_gate_test.go` (tag `m4rostergate`) — loader and shape checks only
- Create: `internal/ingest/testdata/rostergate_shape.yaml` — a three-member fixture used only to exercise the loader

**Interfaces:**
- Produces, consumed by Tasks 3 and 4:
  - `type expectedRoster struct { Capture int64; PeriodKey, GameVersion string; Alliance expectedRosterAlliance; Frames []expectedRosterFrame; Groups []expectedGroup; Members []expectedMember }`
  - `type expectedRosterAlliance struct { Tag, Name string; MemberCount int }`
  - `type expectedRosterFrame struct { Seq int; SHA256 string; OffsetPx int; GroupKey string }`
  - `type expectedGroup struct { Rank, Name string; Total int }`
  - `type expectedMember struct { Rank, Name string; Power float64; Level int; LastActive string; Note string }`
  - `func loadExpectedRoster(t *testing.T, path string) expectedRoster`

- [ ] **Step 1: Write the failing shape test**

Create `internal/ingest/roster_gate_test.go`:

```go
//go:build m4rostergate

// The M4 roster gate: ingest reproduces a hand-transcribed roster capture.
//
// Behind a build tag for the same three reasons gate_test.go is — Postgres via
// internal/dbtest, the blob store holding the capture's frames, and the
// tesseract binary. Run it with `make gate-roster`.
//
// It asserts the four conditions from the design doc's §4. Two of them differ
// from gate-m4's, and the difference is not stylistic: the VS route MATCHES
// into a closed set, and this route CREATES. So the VS gate's "no
// misattribution" becomes "no splits" here, and its "the capture reconciles to
// complete" inverts into "reconciliation reports the shortfall truthfully" --
// a gate demanding `complete` cannot go green while the route is still
// climbing, and a gate that cannot go green is not a ratchet.
//
//  1. >= 95% of transcribed members exist in `members`, in the right group;
//  2. zero splits -- no two member rows claim the same transcribed member,
//     and no member row corresponds to nobody;
//  3. every transcribed member not created produced a review_queue row;
//  4. PerGroup tallies and Status describe what was parsed, truthfully.
package ingest_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// expectedRoster is the hand-transcribed ground truth. See
// fixtures/m4rostergate/README.md for how it is produced and why it is
// transcribed rather than exported.
type expectedRoster struct {
	// Capture is provenance only: the gate seeds its own capture row in
	// lw_manager_test and never looks this id up.
	Capture     int64                  `yaml:"capture"`
	PeriodKey   string                 `yaml:"period_key"`
	GameVersion string                 `yaml:"game_version"`
	Alliance    expectedRosterAlliance `yaml:"alliance"`
	Frames      []expectedRosterFrame  `yaml:"frames"`
	Groups      []expectedGroup        `yaml:"groups"`
	Members     []expectedMember       `yaml:"members"`
}

type expectedRosterAlliance struct {
	Tag  string `yaml:"tag"`
	Name string `yaml:"name"`
	// MemberCount is the "97" of "Members: 97/100" read off the alliance
	// frame. It is the alliance-total reconciliation's ground truth.
	MemberCount int `yaml:"member_count"`
	// Leader occupies the MEMBER LIST screen's banner and has no rank-group
	// row, while every other member has one. That is why the group totals sum
	// to one less than MemberCount, and recording the name is what makes the
	// loader's off-by-one check a statement about this screen's structure
	// rather than a fudge factor.
	Leader string `yaml:"leader"`
}

// expectedRosterFrame names one captured screenshot by content hash. OffsetPx
// is copied from capture_frames, never re-derived: it was measured against the
// frames as captured and ingest turns it into row positions, so a wrong value
// misaligns every row after it. GroupKey is "_alliance_summary" on the one
// frame carrying the "Members: 96/100" line and empty on every member-list
// frame -- roster_capture deliberately asserts no group.
type expectedRosterFrame struct {
	Seq      int    `yaml:"seq"`
	SHA256   string `yaml:"sha256"`
	OffsetPx int    `yaml:"offset_px"`
	GroupKey string `yaml:"group_key"`
}

// expectedGroup is one rank group's own sticky header as a human read it.
// Total is the M of "10/64" -- the group's size, not its online count. It is
// ground truth here rather than something the gate infers, because the header
// count is the structural gate on member creation and is currently the
// route's dominant defect.
// Expanded records whether roster_capture opened this group during the run.
// R1 Danger Zone reads "0/12" and is still COLLAPSED in capture 1's final
// frame, so its twelve members have no rows anywhere in the capture: they are
// a group the capture saw and never opened, not twelve members the pipeline
// lost. The distinction is the whole reason this field exists -- without it a
// collapsed group is indistinguishable from a catastrophic read failure.
type expectedGroup struct {
	Rank     string `yaml:"rank"`
	Name     string `yaml:"name"`
	Total    int    `yaml:"total"`
	Expanded bool   `yaml:"expanded"`
}

// expectedMember is one member row as a human read it, at full resolution,
// across every frame the member appears in.
//
// Power and Level are transcribed though the gate does not assert them: both
// fields yield zero facts today and are deferred to M6, and transcription is
// the expensive part of this fixture -- doing it twice would be indefensible.
// The probes read them.
//
// LastActive is the string the row shows: "Online", or an elapsed time like
// "3h ago". Note carries a transcriber's remark for a glyph that is a best
// reading rather than a certain one, on the model of the VS fixture's
// thirteen decorated names. A member carrying a Note is still asserted; the
// note is what a failure should be re-read against before the pipeline is
// blamed.
type expectedMember struct {
	Rank       string  `yaml:"rank"`
	Name       string  `yaml:"name"`
	Power      float64 `yaml:"power"`
	Level      int     `yaml:"level"`
	LastActive string  `yaml:"last_active"`
	Note       string  `yaml:"note,omitempty"`
}

// gateRosterMinMembers keeps a thin transcription from passing vacuously, the
// same guard gate_test.go's gateMinRows is and for the same reason: below
// twenty, one bad row already breaks a 95% threshold and the percentage stops
// meaning anything.
const gateRosterMinMembers = 20

func TestRosterGateFixtureShape(t *testing.T) {
	exp := loadExpectedRoster(t, filepath.Join("testdata", "rostergate_shape.yaml"))
	if len(exp.Members) != 3 {
		t.Fatalf("shape fixture has %d members, want 3", len(exp.Members))
	}
	if exp.Groups[0].Total != 2 {
		t.Fatalf("group %q total = %d, want 2", exp.Groups[0].Rank, exp.Groups[0].Total)
	}
}

// loadExpectedRoster reads and validates the ground truth, skipping when it
// has not been transcribed yet -- the same shape as the M1 gate skipping an
// unpulled corpus, and for the same reason: the artifact is deliberately not
// generated.
//
// Every check below is a transcription mistake that would otherwise produce a
// confident, meaningless verdict.
func loadExpectedRoster(t *testing.T, path string) expectedRoster {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no hand-transcribed roster at %s; transcribe one first (see fixtures/m4rostergate/README.md)", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var exp expectedRoster
	if err := yaml.Unmarshal(data, &exp); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	switch {
	case exp.PeriodKey == "":
		t.Fatalf("%s: period_key is required", path)
	case exp.GameVersion == "":
		t.Fatalf("%s: game_version is required; it is what explains a gate that used to pass", path)
	case exp.Alliance.Tag == "" || exp.Alliance.Name == "":
		t.Fatalf("%s: alliance tag and name are required", path)
	case exp.Alliance.MemberCount <= 0:
		t.Fatalf("%s: alliance member_count is required; it is the alliance-total reconciliation's ground truth", path)
	case len(exp.Frames) == 0:
		t.Fatalf("%s: no frames listed", path)
	case len(exp.Groups) == 0:
		t.Fatalf("%s: no groups listed; group headers are ground truth here, not inferred", path)
	}

	// The group totals plus the leader must equal the alliance count. This is
	// the transcription's own internal check and it is worth more than it
	// looks: it is the one arithmetic relation the screen states twice, so a
	// disagreement means a header was misread or a group was missed entirely,
	// and either would silently weaken every condition the gate then asserts.
	//
	// Plus the leader, not equal outright: the leader occupies the MEMBER LIST
	// banner and has no rank-group row, while every other member has one. On
	// capture 1 that is 9 + 64 + 11 + 12 = 96 against a "Members: 97/100"
	// line. An alliance where that relation does not hold fails here loudly,
	// which is the right failure -- it means the screen's structure changed
	// and every group-keyed assumption in this package needs re-reading.
	sum := 0
	seenRank := map[string]bool{}
	for _, g := range exp.Groups {
		if g.Rank == "" {
			t.Fatalf("%s: a group has no rank", path)
		}
		if seenRank[g.Rank] {
			t.Fatalf("%s: rank %q appears twice; groups are keyed by rank", path, g.Rank)
		}
		seenRank[g.Rank] = true
		if g.Total <= 0 {
			t.Fatalf("%s: group %q has total %d, want a positive size", path, g.Rank, g.Total)
		}
		sum += g.Total
	}
	if exp.Alliance.Leader == "" {
		t.Fatalf("%s: alliance leader is required; the sum check below is a statement about the leader having no group row", path)
	}
	if sum+1 != exp.Alliance.MemberCount {
		t.Fatalf("%s: group totals sum to %d, +1 for leader %q = %d, but the alliance frame reads %d; a header was misread, a group is missing, or this screen no longer puts exactly one member in the banner",
			path, sum, exp.Alliance.Leader, sum+1, exp.Alliance.MemberCount)
	}

	// Every transcribed member must belong to a group the capture EXPANDED.
	// A member listed under a collapsed group is a transcription error by
	// construction: the capture contains no row for them to have been read
	// from, so asserting them would fail the gate on frames that do not exist.
	expanded := map[string]bool{}
	for _, g := range exp.Groups {
		expanded[g.Rank] = g.Expanded
	}

	seenName := map[string]bool{}
	for _, m := range exp.Members {
		if m.Name == "" {
			t.Fatalf("%s: a member in group %q has no name", path, m.Rank)
		}
		if !seenRank[m.Rank] {
			t.Fatalf("%s: member %q is in group %q, which is not in the groups list", path, m.Name, m.Rank)
		}
		if !expanded[m.Rank] {
			t.Fatalf("%s: member %q is in group %q, which this capture never expanded; there is no row to have read them from", path, m.Name, m.Rank)
		}
		if seenName[m.Name] {
			t.Fatalf("%s: %q is transcribed twice; the gate keys members by name, so a duplicate cannot be scored", path, m.Name)
		}
		seenName[m.Name] = true
	}

	sort.Slice(exp.Frames, func(i, j int) bool { return exp.Frames[i].Seq < exp.Frames[j].Seq })
	return exp
}
```

- [ ] **Step 2: Run it and confirm it skips**

```bash
go test -tags m4rostergate ./internal/ingest/ -run TestRosterGateFixtureShape -v
```

Expected: SKIP with `no hand-transcribed roster at testdata/rostergate_shape.yaml`. The skip is the correct failure — the file does not exist yet.

- [ ] **Step 3: Write the shape fixture**

Create `internal/ingest/testdata/rostergate_shape.yaml`. It is deliberately not real data: it exists only to exercise `loadExpectedRoster`'s guards.

```yaml
# A three-member fixture that exists only to exercise loadExpectedRoster's
# shape checks. It is NOT ground truth and is NOT the gate's input -- the gate
# reads fixtures/m4rostergate/expected.yaml. Keeping this separate is what lets
# the loader's guards be tested without a 96-member transcription and without
# the blob store.
capture: 0
period_key: "2026-W33"
game_version: "0.0.0"

alliance:
  tag: "TEST"
  name: "Shape Fixture"
  # Alpha 2 + Beta 1 + Gamma 2 = 5 grouped, + 1 leader in the banner = 6.
  # Only 3 are transcribed as members: Gamma was never expanded.
  member_count: 6
  leader: "TheLeader"

frames:
  - seq: 0
    sha256: "0000000000000000000000000000000000000000000000000000000000000000"
    offset_px: 0
    group_key: "_alliance_summary"

groups:
  - rank: "R4"
    name: "Alpha"
    total: 2
    expanded: true
  - rank: "R3"
    name: "Beta"
    total: 1
    expanded: true
  # A collapsed group: its header is transcribed and states a real size, its
  # members are not transcribed, because the capture holds no rows for them.
  # This is R1 Danger Zone's shape exactly -- R1's header reads "0/12", where
  # the 0 is how many are online and the 12 is the group's size.
  - rank: "R2"
    name: "Gamma"
    total: 2
    expanded: false

members:
  - rank: "R4"
    name: "MemberOne"
    power: 211500000
    level: 34
    last_active: "Online"
  - rank: "R4"
    name: "MemberTwo"
    power: 189200000
    level: 32
    last_active: "3h ago"
  - rank: "R3"
    name: "MemberThree"
    power: 230800000
    level: 35
    last_active: "Online"
    note: "trailing glyph is a best reading"
```

- [ ] **Step 4: Run the test and confirm it passes**

```bash
go test -tags m4rostergate ./internal/ingest/ -run TestRosterGateFixtureShape -v
```

Expected: PASS.

- [ ] **Step 5: Mutation-check the sum guard**

Change Beta's `total: 1` to `total: 2` and re-run. Expected: FAIL naming the sum, the leader and the alliance count. Restore it.

Then set Gamma's `expanded: false` to `true` and add a member under `R2`; re-run and confirm it PASSES, then set it back to `false` and confirm the same fixture now FAILS with `which this capture never expanded`. Both guards are the transcription's only self-checks, and a guard that cannot fail is worse than no guard.

The collapsed group carries a real `total`, exactly as R1 does. A group header states its size whether or not the group is open — R1's reads `0/12`, where the 0 is the online count and the 12 is the size — so `total` stays strictly positive for every group and the guard is `<= 0`. What a collapsed group lacks is rows, not a count.

- [ ] **Step 6: Write the fixture README**

Create `fixtures/m4rostergate/README.md`, modelled on `fixtures/m4gate/README.md`. It must cover, in this order: why it is transcribed rather than exported (checking a pipeline against its own output proves nothing, and this project has already paid for that lesson once — the `vs` label that stayed self-consistently wrong for three weeks); the transcription rule (**a value is recorded only if it reads identically in every frame it appears in**, which capture 1's ~3.8x overlap makes a real check); how to produce one (read frames from the blob store, `offset_px` copied from `capture_frames` and never re-derived, `sha256` is the whole reference since keys derive from the digest via `blob.Key`); the four conditions the gate then asserts; and that frames are gitignored while the `.yaml` commits normally.

It must also state plainly what §3 of the design doc argues: what makes this ground truth is that it is read off the pixels through a path sharing nothing with the pipeline it judges, and that the eye-check which failed twice in this milestone was a check of *a crop* — a rectangle whose contents the reader already knew — not of a full frame.

- [ ] **Step 7: Confirm the untagged suite is untouched, then commit**

```bash
go test ./...
git add internal/ingest/roster_gate_test.go internal/ingest/testdata/rostergate_shape.yaml fixtures/m4rostergate/README.md
git commit -m "Settle the roster gate's fixture format before transcribing into it

The roster route has no ground truth. Building the loader and its guards first,
against a deliberate three-member fixture, means a format mistake costs three
rows rather than ninety-six.

The guards are all transcription mistakes that would otherwise produce a
confident, meaningless verdict: a missing game_version, a duplicate name the
gate could not score, a member in a group that is not listed. The one worth
naming is the sum check -- group totals must equal the alliance frame's own
member count, which is the single arithmetic relation this screen states twice,
so a disagreement means a header was misread or a group was missed. It is
mutation-checked, because a guard that cannot fail is worse than no guard.

Groups are ground truth here rather than inferred. The header count is the
structural gate on member creation and is currently the route's dominant
defect, so the gate cannot be allowed to take the pipeline's word for it."
```

---

### Task 3: Transcribe capture 1

The long pole. **75 members** across 61 member-list frames, read at full resolution, with cross-frame agreement required.

**What capture 1 contains, established during pre-flight — do not re-derive it, but do verify it as you go.** The alliance frame (`seq 0`) reads `[OrCa] Organized Chaos`, leader `RobElr`, `Members: 97/100`. There are four rank groups:

| rank | name | total | expanded in this capture? |
|---|---|---|---|
| R4 | This Is It | 9 | **no — collapsed, up chevron, no rows follow** |
| R3 | Footloose | 64 | yes |
| R2 | I'm Alright | 11 | yes |
| R1 | Danger Zone | 12 | **no — still collapsed in the final frame** |

`9 + 64 + 11 + 12 = 96`, plus leader `RobElr` in the banner = 97. R5 is not a rank group; it is the leader's badge in the banner, which is why `rankBadgeOrder` covers only `R1`–`R4`.

**So transcribe 75 members — R3's 64 and R2's 11 — and no R4 or R1 members**, because the capture holds no rows for them. Transcribe **all four group headers**, R4's and R1's included at their true totals with `expanded: false`. The loader rejects a member listed under a group it was told was collapsed.

If any of the above disagrees with the pixels, **the pixels win**: correct it, say so in the report, and note it in the file's header comment.

**Files:**
- Create: `fixtures/m4rostergate/expected.yaml`

**Interfaces:**
- Consumes: `expectedRoster` and its sub-types from Task 2; the frame list already committed at `fixtures/m4roster/frames.yaml` (62 entries, each with `seq`, `sha256`, `offset_px`, `group_key`).
- Produces: the file every later task reads.

- [ ] **Step 1: Copy the frame list verbatim**

`fixtures/m4roster/frames.yaml` already carries all 62 frames with `offset_px` and `group_key`, transcribed from `capture_frames` when the probe was built. Copy its `frames:` block into `expected.yaml` unchanged.

**Do not re-derive `offset_px`.** It was measured against the frames as captured; ingest turns it into row positions, so a wrong value misaligns every row after it.

- [ ] **Step 2: Materialize the frames for reading**

```bash
cd /home/tom/Projects/lw-manager
mkdir -p /tmp/rostergate
python3 - <<'PY'
import re, pathlib, shutil
src = pathlib.Path("fixtures/m4roster/frames.yaml").read_text()
entries = re.findall(r'- seq: (\d+)\s+sha256: "([0-9a-f]{64})"', src)
for seq, h in entries:
    blob = pathlib.Path("data/blobs/sha256")/h[:2]/h[2:4]/h
    if blob.exists():
        shutil.copy(blob, f"/tmp/rostergate/seq{int(seq):02d}.png")
print(f"{len(entries)} frames listed")
PY
ls /tmp/rostergate | head
```

- [ ] **Step 3: Read the alliance frame first**

`seq 0` carries `group_key: "_alliance_summary"` and the `Members: 96/100` line. Record `alliance.member_count` from it, and the alliance tag and name.

- [ ] **Step 4: Confirm the group headers off the pixels**

Walk every member-list frame and record each rank group's sticky header: rank badge, group name, the `M` of its `N/M` count, and whether the group is expanded. The table above is what pre-flight read; confirm each row rather than copying it.

The loader's sum check rejects the file unless the group totals plus the leader equal `alliance.member_count`, which is the arithmetic this screen states twice.

- [ ] **Step 5: Transcribe members, requiring cross-frame agreement**

For each member: rank group, name, power (as a number — `Power: 211.5M` becomes `211500000`), level, and the `last_active` string. That last field is the text at the row's top right: `Online` when the member is online, otherwise an elapsed time — `2h ago`, `13h ago`, `1d ago`.

**The rule: a value goes in only if it reads identically in every frame the member appears in.** At ~3.8x overlap most members appear in three or four frames. Where frames disagree, or a glyph is genuinely ambiguous, record your best reading **and** a `note:` saying so — the way the VS fixture marks its thirteen decorated names as "a best reading rather than a certain one." Do not drop the member; dropping is editing the ground truth to suit the parser.

Names carrying decoration (`ΔΚΔŽΔ`, `Danny 狂`, `ϟϟ Leo ϟϟ`, `Aureum ⊂👑`) are transcribed as closely as the rendering allows. The ASCII cores are what matter and are not in doubt.

- [ ] **Step 6: Write the file with a header comment recording the method**

The header must state: what was transcribed and from what, that it was read off the pixels and not from `control ingest`, the cross-frame agreement rule, how many members carry a `note` and why, and any group whose header could not be read confidently.

- [ ] **Step 7: Validate it through the loader**

Point the shape test at the real file temporarily, or add a second test — the simpler move is a one-off run:

```bash
LW_BLOB_FS_ROOT="$(pwd)/data/blobs" go test -tags m4rostergate ./internal/ingest/ -run TestRosterGateFixtureShape -v
```

Then change `TestRosterGateFixtureShape` to also load `../../fixtures/m4rostergate/expected.yaml` and assert `len(exp.Members) >= gateRosterMinMembers`, skipping if the file is absent so a fresh clone still builds:

```go
func TestRosterGateGroundTruthShape(t *testing.T) {
	exp := loadExpectedRoster(t, filepath.Join("..", "..", "fixtures", "m4rostergate", "expected.yaml"))
	if len(exp.Members) < gateRosterMinMembers {
		t.Fatalf("%d members transcribed, want at least %d; fewer cannot support a 95%% threshold",
			len(exp.Members), gateRosterMinMembers)
	}
	t.Logf("ground truth: %d members across %d groups, alliance count %d, game version %s",
		len(exp.Members), len(exp.Groups), exp.Alliance.MemberCount, exp.GameVersion)
}
```

- [ ] **Step 8: Sanity-check against what the pipeline already produced — as a review aid, not a source**

The 57 members currently in the dev database were produced by the pipeline and **must not** be copied into the fixture. But a diff between them and the transcription is a cheap way to catch a transcription slip: a name the pipeline read exactly and you read differently is worth re-opening the frame for.

All 57 are R3, so this checks about two-thirds of R3 and nothing in R4 or R2. Expect it to say nothing about the other 20 members — that is the check working, not failing.

```bash
docker compose exec -T postgres psql -U lw -d lw_manager -t -A -c "SELECT name FROM members ORDER BY name;"
```

Re-read the frame for any disagreement. Resolve it from the pixels, in both directions — expect several where the pipeline is wrong (`ALBANSO`, `AASIAA`, `Deliot =`, `Kyra ©` are known confusable or decoration misreads).

- [ ] **Step 9: Commit**

```bash
git add fixtures/m4rostergate/expected.yaml internal/ingest/roster_gate_test.go
git commit -m "Transcribe capture 1: the roster route's first ground truth

75 members across 61 member-list frames, read at full resolution out of the
blob store, one frame at a time. 75 rather than the alliance's 97 because two
of the four rank groups were never opened -- R4 This Is It and R1 Danger Zone
both carry the up chevron and are followed by no rows anywhere in the capture
-- and the leader occupies the banner rather than any group's list. Both are recorded in the fixture as what they are, so the
gate measures the pipeline against frames that exist. Not from control ingest's summary, not from a
members query, not from a previous gate run -- checking a pipeline against its
own output proves nothing, and this project has already paid for that lesson
when a screen labelled `vs` stayed self-consistently wrong for three weeks
because nothing in the corpus could disagree with it.

A value is recorded only where it reads identically in every frame the member
appears in. Capture 1 carries ~3.8x overlap, so that is a real check rather
than a formality. Glyphs that are a best reading rather than a certain one
carry a note and are still asserted -- dropping them would be editing the
ground truth to suit the parser.

The frame list is copied verbatim from fixtures/m4roster/frames.yaml, offset_px
included and not re-derived: it was measured against the frames as captured,
and ingest turns it into row positions."
```

---

### Task 4: The gate, and its red baseline

**Files:**
- Modify: `internal/ingest/roster.go` — expose `GroupTally.MatchedOrCreated`
- Modify: `internal/ingest/roster_gate_test.go` — the four conditions
- Modify: `Makefile` — `gate-roster`

**Interfaces:**
- Consumes: `loadExpectedRoster` (Task 2), `fixtures/m4rostergate/expected.yaml` (Task 3), `ingest.New(store, blobs, engine).IngestRoster(ctx, captureID, periodKey) (RosterResult, error)`, `dbtest.Prepare`, `dbtest.SeedAccount`, `pool.UpsertAlliance`, `pool.RecordCapture`, `pool.PendingReviews`, `pool.ListMembers`, `roster.Match`, `roster.AutoAccept`.
- Produces: `GroupTally.MatchedOrCreated int`, and `make gate-roster`.

- [ ] **Step 1: Expose the per-group created count**

`groupTracker.matchedOrCreated` already exists (`roster.go:445`) and gates member creation, but `GroupTally` exposes only `Expected`, `Parsed` and `Name`. `Parsed` counts row bands that reached OCR, which is not the same claim as "members this group actually contributed" — the gate's condition 4 needs the latter.

In `internal/ingest/roster.go`, add to `GroupTally`:

```go
	// MatchedOrCreated is how many of this group's rows ended as a member --
	// matched to an existing one or created. It is deliberately distinct from
	// Parsed, which counts row bands that reached OCR: a band that was read
	// and then failed to match is parsed and contributes no member, so the
	// two numbers answer different questions and the gap between them is
	// exactly the review queue. Reconciliation keys on Parsed, because it is
	// asking whether the scroll saw the whole group; the gate keys on this,
	// because it is asking what the group yielded.
	MatchedOrCreated int
```

Populate it wherever `gt.matchedOrCreated++` occurs (`roster.go:865` and `roster.go:909`) — mirror the existing `tally := run.res.PerGroup[groupKey]; tally.X++; run.res.PerGroup[groupKey] = tally` pattern used at `roster.go:717-720`. The group key at those sites is available as `groupKey` on `processRow`.

Surface it in `cmd/control/ingest.go:228-230` alongside `parsed=` and `expected=`, as `created=`.

- [ ] **Step 2: Write the four conditions**

Append to `internal/ingest/roster_gate_test.go`. Note that members are **not** seeded — unlike the VS gate, this route creates them, and seeding would measure nothing.

```go
func TestM4RosterGate(t *testing.T) {
	ctx := context.Background()

	exp := loadExpectedRoster(t, filepath.Join("..", "..", "fixtures", "m4rostergate", "expected.yaml"))
	if len(exp.Members) < gateRosterMinMembers {
		t.Fatalf("%d members transcribed, want at least %d", len(exp.Members), gateRosterMinMembers)
	}
	blobs := rosterGateBlobs(t, ctx, exp)

	engine := ocr.NewTesseractEngine()
	if !engine.Available() {
		t.Skip("tesseract is not on PATH; install it with `apt install tesseract-ocr tesseract-ocr-eng` (Debian/Ubuntu) and re-run")
	}

	pool := gatePool(t, ctx)
	captureID := seedRosterCapture(t, ctx, pool, exp)

	res, err := ingest.New(pool, blobs, engine).IngestRoster(ctx, captureID, exp.PeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}

	allianceID, err := pool.CurrentAllianceID(ctx)
	if err != nil {
		t.Fatalf("CurrentAllianceID: %v", err)
	}
	created, err := pool.ListMembers(ctx, allianceID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}

	// Correspondence between a created member row and a transcribed member is
	// judged by roster.Rank at AutoAccept -- the same scorer the pipeline
	// uses, deliberately. A cosmetically wrong display name is recoverable:
	// ALBANSO for a real ALBAN80 is a documented confusable, so a later VS
	// ingest reading the name correctly matches it to that same row and the
	// facts land in one place. Scoring by exact string would fail the gate on
	// a defect that costs nothing, and hide the one that costs everything.
	//
	// roster.Member carries no NameNormalized field: TokenSetRatio normalizes
	// internally, so the truth set is built from display names alone. IDs are
	// synthetic indices -- nothing here keys on them, and Rank requires the
	// field to exist.
	truth := make([]roster.Member, 0, len(exp.Members))
	for i, m := range exp.Members {
		truth = append(truth, roster.Member{ID: int64(i + 1), Name: m.Name})
	}

	claimedBy := map[string][]string{} // transcribed name -> member rows claiming it
	var orphans []string
	for _, m := range created {
		cands := roster.Rank(m.Name, truth) // sorted best-first
		switch {
		case len(cands) == 0:
			t.Fatalf("roster.Rank returned nothing for %q against %d transcribed members", m.Name, len(truth))
		case cands[0].Score < roster.AutoAccept:
			orphans = append(orphans, fmt.Sprintf("%q (best %q at %d, below AutoAccept %d)",
				m.Name, cands[0].Name, cands[0].Score, roster.AutoAccept))
		default:
			claimedBy[cands[0].Name] = append(claimedBy[cands[0].Name], m.Name)
		}
	}

	// --- Condition 1: coverage.
	rankByName := map[string]string{}
	for _, m := range exp.Members {
		rankByName[m.Name] = m.Rank
	}
	rankOf := map[string]string{}
	for _, m := range created {
		rankOf[m.Name] = m.Rank
	}

	var missing []string
	covered := 0
	for _, m := range exp.Members {
		rows := claimedBy[m.Name]
		if len(rows) == 0 {
			missing = append(missing, fmt.Sprintf("%s %q: never created", m.Rank, m.Name))
			continue
		}
		if got := rankOf[rows[0]]; got != m.Rank {
			missing = append(missing, fmt.Sprintf("%s %q: created in group %q", m.Rank, m.Name, got))
			continue
		}
		covered++
	}
	coverage := float64(covered) / float64(len(exp.Members))
	if coverage < gateRosterCoverage {
		sort.Strings(missing)
		t.Errorf("roster gate condition 1: %d/%d members covered (%.4f) is below %.2f\n\n%s",
			covered, len(exp.Members), coverage, gateRosterCoverage, join(missing))
	}

	// --- Condition 2: zero splits.
	//
	// Two failures, one hard zero each. An orphan is a member row matching
	// nobody on the roster -- a person invented from a misread. A split is the
	// same person minted twice under two different reads, and it is the worse
	// of the two: their facts divide across two rows and no review-queue
	// resolution rejoins them. This is the roster route's equivalent of a VS
	// misattribution, and like it, it gets a hard zero rather than a
	// percentage.
	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf("roster gate condition 2: %d member rows correspond to nobody transcribed\n\n%s", len(orphans), join(orphans))
	}
	var splits []string
	for name, rows := range claimedBy {
		if len(rows) > 1 {
			sort.Strings(rows)
			splits = append(splits, fmt.Sprintf("%q claimed by %d rows: %v", name, len(rows), rows))
		}
	}
	if len(splits) > 0 {
		sort.Strings(splits)
		t.Errorf("roster gate condition 2: %d transcribed members are split across two member rows\n\n%s", len(splits), join(splits))
	}

	// --- Condition 3: nothing dropped silently.
	//
	// Counts rather than pairing each miss to its own review row, for the same
	// reason gate_test.go's condition 2 does: a review row records a screen
	// position and its raw text, not a member, because an unmatched name has
	// no member to key on. It still catches the failure the condition exists
	// for -- a member missed with no review row at all -- and it still cannot
	// catch reviews raised for unrelated reasons padding the total, so read
	// the two numbers together rather than the verdict alone.
	pending, err := pool.PendingReviews(ctx)
	if err != nil {
		t.Fatalf("PendingReviews: %v", err)
	}
	queued := 0
	for _, item := range pending {
		if item.CaptureID == captureID {
			queued++
		}
	}
	if queued < len(missing) {
		t.Errorf("roster gate condition 3: %d members are missing but only %d rows reached the review queue; %d were dropped silently",
			len(missing), queued, len(missing)-queued)
	}

	// --- Condition 4: reconciliation reports truthfully.
	//
	// NOT "the capture is complete". Reconciliation marks any group-count
	// mismatch partial, so a gate demanding complete cannot go green while the
	// route is still climbing, and a gate that cannot go green is not a
	// ratchet. Reconciliation is a reporting mechanism; what is worth
	// asserting is that its report is honest.
	// R1 Danger Zone is transcribed with expanded: false and no members. Its
	// header is legible, so a healthy pipeline gives it a tally reading
	// Expected 12 / Parsed 0 / MatchedOrCreated 0, which makes wholeEverywhere
	// false and the capture correctly `partial`. That is not a failure to
	// excuse -- it is precisely what condition 4 exists to assert, and a
	// capture that reported `complete` with a group it never opened would be
	// the defect.
	var lies []string
	wholeEverywhere := true
	for _, g := range exp.Groups {
		tally, seen := res.PerGroup[g.Rank]
		if !seen {
			wholeEverywhere = false
			// A group whose header never parsed leaves no tally at all. That
			// is permitted -- it is what the route does today -- but only if
			// it is visible in the queue rather than silently absent.
			if queued == 0 {
				lies = append(lies, fmt.Sprintf("group %s (%q, %d members) produced no tally and no review row", g.Rank, g.Name, g.Total))
			}
			continue
		}
		if tally.Expected != g.Total {
			lies = append(lies, fmt.Sprintf("group %s: reported expected=%d, transcribed total is %d", g.Rank, tally.Expected, g.Total))
		}
		inGroup := 0
		for _, m := range exp.Members {
			if m.Rank == g.Rank && len(claimedBy[m.Name]) > 0 {
				inGroup++
			}
		}
		if tally.MatchedOrCreated != inGroup {
			lies = append(lies, fmt.Sprintf("group %s: reported created=%d, but %d of its transcribed members are in members", g.Rank, tally.MatchedOrCreated, inGroup))
		}
		if tally.Parsed != tally.Expected {
			wholeEverywhere = false
		}
	}
	wantStatus := "partial"
	if wholeEverywhere {
		wantStatus = "complete"
	}
	if res.Status != wantStatus {
		lies = append(lies, fmt.Sprintf("status is %q; every group whole = %v, so it should be %q", res.Status, wholeEverywhere, wantStatus))
	}
	if len(lies) > 0 {
		sort.Strings(lies)
		t.Errorf("roster gate condition 4: reconciliation does not describe what was parsed\n\n%s", join(lies))
	}

	t.Logf("roster gate: %d/%d members covered, orphans=%d splits=%d matched=%d created=%d queued=%d status=%s (game version %s)",
		covered, len(exp.Members), len(orphans), len(splits), res.Matched, res.Created, res.Queued, res.Status, exp.GameVersion)
}
```

Add the coverage constant beside `gateRosterMinMembers`:

```go
// gateRosterCoverage is taken from gate-m4's bar rather than derived from what
// this route currently does. That ordering is the point: the VS gate's 95%
// came from the design doc and the pipeline had to climb 63/86 to 85/86 to
// reach it. A bar set after seeing the number is a bar fitted to the pipeline
// it was supposed to judge.
//
// It applies to TRANSCRIBED members (84 on capture 1), not to the alliance's
// own count (97). The two differ by R1 Danger Zone's collapsed 12 and the
// leader's banner row, and neither is anything the pipeline could read from
// these pixels -- scoring against 97 would be scoring against frames the
// capture does not contain, and would make the bar unreachable by
// construction at anything above 86.6%.
//
// The cost is worth stating rather than burying: R1 and the leader are never
// exercised here, so a defect specific to a collapsed group or to the banner
// goes uncaught until a capture that expands R1 exists.
const gateRosterCoverage = 0.95
```

- [ ] **Step 3: Extract the shared gate helpers, then write the seeding helper**

`join()`, `gatePool()` and `envOr()` live in `gate_test.go`, which carries `//go:build m4gate`. Building with `-tags m4rostergate` does not compile that file, so the roster gate cannot call them — and redeclaring them would collide whenever both tags are set. Move all three into a new `internal/ingest/gate_shared_test.go`:

```go
//go:build m4gate || m4rostergate

// Helpers shared by the two M4 gates. They live behind a disjunction of both
// tags rather than in either gate's own file because each gate is compiled
// alone: -tags m4gate does not build the roster gate and vice versa, so a
// helper declared in one is invisible to the other and declaring it in both
// collides when someone builds with both.
package ingest_test
```

Move `join`, `gatePool` and `envOr` into it verbatim, delete them from `gate_test.go`, and confirm the VS gate still builds and runs before going further:

```bash
make gate-m4 2>&1 | tail -20
```

Then add the roster seeding helper:

```go
// seedRosterCapture builds the database state one real roster capture would
// have left behind: an alliance, a screenshot row per frame pointing at the
// content-addressed blob, and a capture referencing them in scroll order.
//
// Members are deliberately NOT seeded. The VS gate seeds them because IngestVS
// never creates one; this route's entire job is to create them, and seeding
// would mean the gate measured nothing while reporting a number.
//
// The alliance is scoped to this run for the reason gate_test.go records:
// lw_manager_test is never truncated between runs, so a shared alliance
// accumulates every previous run's members. It also has to be the MOST
// RECENTLY observed alliance, because IngestRoster resolves its alliance via
// CurrentAllianceID, which is `ORDER BY observed_at DESC LIMIT 1`.
//
// complete=false: capture 1 is a partial capture and the fixture transcribes
// it as one. Condition 4 asserts that reconciliation says so, rather than
// asserting a status the capture never had.
func seedRosterCapture(t *testing.T, ctx context.Context, pool *db.Pool, exp expectedRoster) int64 {
	t.Helper()

	accountID := dbtest.SeedAccount(ctx, t, pool)

	if _, err := pool.UpsertAlliance(ctx, db.Alliance{
		Tag:         fmt.Sprintf("%s-%d", exp.Alliance.Tag, accountID),
		Name:        fmt.Sprintf("%s (roster gate run %d)", exp.Alliance.Name, accountID),
		MemberCount: exp.Alliance.MemberCount,
	}); err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}

	frames := make([]db.CaptureFrameInput, len(exp.Frames))
	for i, f := range exp.Frames {
		var shotID int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO screenshots (account_id, captured_at, object_key, sha256)
			 VALUES ($1, now(), $2, $3) RETURNING id`,
			accountID, blob.Key(f.SHA256), f.SHA256).Scan(&shotID); err != nil {
			t.Fatalf("seeding screenshot for frame %d: %v", f.Seq, err)
		}
		frames[i] = db.CaptureFrameInput{
			ScreenshotID: shotID, Seq: f.Seq, OffsetPx: f.OffsetPx, GroupKey: f.GroupKey,
		}
	}

	if err := pool.RecordCapture(ctx, accountID, "roster", frames, false); err != nil {
		t.Fatalf("RecordCapture: %v", err)
	}

	var captureID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM captures WHERE account_id = $1 AND route = 'roster'`, accountID,
	).Scan(&captureID); err != nil {
		t.Fatalf("reading back the seeded capture: %v", err)
	}
	return captureID
}

// rosterGateBlobs opens the configured blob store and confirms every
// transcribed frame is in it. Only cfg.Blob is used: the database side goes
// through internal/dbtest, which reads LW_TEST_DATABASE_URL and never the
// application's LW_DATABASE_URL.
func rosterGateBlobs(t *testing.T, ctx context.Context, exp expectedRoster) blob.Store {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load(): %v", err)
	}
	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		t.Fatalf("opening blob store (%s): %v", cfg.Blob.Backend, err)
	}
	for _, f := range exp.Frames {
		ok, err := blobs.Exists(ctx, blob.Key(f.SHA256))
		if err != nil {
			t.Fatalf("checking blob for frame %d: %v", f.Seq, err)
		}
		if !ok {
			t.Skipf("frame %d (%s) is not in the %s blob store; set LW_BLOB_FS_ROOT to an absolute path (see CLAUDE.md)",
				f.Seq, f.SHA256, cfg.Blob.Backend)
		}
	}
	return blobs
}
```

`db.CaptureFrameInput` carries `GroupKey` (`internal/db/analytics.go:311-316`), so the literal above is correct as written. `roster_capture` leaves it empty on every member-list frame and sets it only on the alliance-summary frame.

- [ ] **Step 4: Add the Makefile target**

```makefile
# The M4 roster gate: ingest reproduces a hand-transcribed roster capture.
#
# Needs Postgres (via internal/dbtest, never the dev database), the blob store
# holding capture 1's frames, and tesseract. Skips when the transcription is
# absent or a frame is missing from the store.
#
# LW_BLOB_FS_ROOT must be ABSOLUTE: go test runs each package binary in its own
# source directory, so the fs backend's relative ./data/blobs would resolve
# under internal/ingest and find nothing.
gate-roster: LW_BLOB_FS_ROOT ?= $(CURDIR)/data/blobs
gate-roster:
	LW_BLOB_FS_ROOT="$(LW_BLOB_FS_ROOT)" $(GO) test -tags m4rostergate -count=1 -v -timeout 20m ./internal/ingest/
.PHONY: gate-roster
```

- [ ] **Step 5: Run it and record the red baseline**

```bash
make gate-roster 2>&1 | tee /tmp/roster-gate-baseline.txt
```

Expected: **FAIL.** Conditions 1, 2 and 4 should all be red — roughly 57 of 96 covered, several orphans among them (`ALBANSO`, `AASIAA`, `Deliot =`, `Kyra ©`), and four groups with no tally at all. Read the output rather than the verdict: the per-group condition-4 lines are the diagnosis every later task works from.

If it passes on the first run, something is wrong — either members were seeded, or the fixture was derived from the pipeline. Investigate before proceeding.

- [ ] **Step 6: Verify the untagged suite still passes**

```bash
go test ./...
```

The `GroupTally.MatchedOrCreated` addition touches `roster.go` and `cmd/control/ingest.go`, both covered by untagged tests.

- [ ] **Step 7: Commit, with the baseline in the message**

```bash
git add internal/ingest/roster_gate_test.go internal/ingest/roster.go cmd/control/ingest.go Makefile
git commit -m "Gate the roster route, and record where it actually stands

Four conditions, two of which differ from gate-m4's because this route fails
differently. The VS route matches into a closed set; this one CREATES. So
'no misattribution' becomes 'no splits' -- correspondence judged by
roster.Match at AutoAccept, with no two member rows allowed to claim the same
transcribed member, since a cosmetically wrong display name is recoverable and
a person minted twice is not. And 'the capture reconciles to complete' inverts
into 'reconciliation reports the shortfall truthfully', because a gate
demanding complete cannot go green while the route is still climbing, and a
gate that cannot go green is not a ratchet.

GroupTally gains MatchedOrCreated. Parsed counts row bands that reached OCR,
which is a different claim from what a group yielded -- the gap between them is
exactly the review queue -- and condition 4 needs the second.

Members are deliberately not seeded. Seeding them is right for the VS gate,
which never creates one, and would mean this gate measured nothing.

First run, recorded before any fix, because a gate whose first run is green was
fitted to the pipeline it was meant to judge:

<paste the baseline line from the run here>"
```

---

### Task 5: Probe modes for the header, power and level

Three fields have never been measured. Before any of them is changed, each gets an instrument — the rule the roster name crop's history exists to enforce.

**Files:**
- Modify: `internal/ingest/zz_roster_probe_test.go`
- Modify: `Makefile` — document the new flags

**Interfaces:**
- Consumes: `loadRosterFrames(t, ctx) []rosterLoadedFrame`, `reportRosterInkProfile`, `scanlineMedian`, `colourDistance`, `SegmentRows`, `groupHeaderRegion`, `groupHeaderOptions`, `groupHeaderSpec`, `parseGroupHeader`, `ParsePower`, `ParseLevel`, all already in package `ingest`.
- Produces: flags `-roster.header`, `-roster.headerink`, `-roster.power`, `-roster.level`.

- [ ] **Step 1: Add the flags**

Beside the existing block at `zz_roster_probe_test.go:81`:

```go
	rosterBadge = flag.Bool("roster.badge", false,
		"report matchRankBadge's per-frame verdict: winning rank, runner-up, gap, against the fixture's own group")
	rosterHeader = flag.Bool("roster.header", false,
		"report what each frame's group header reads, its parsed N/M, and the transcribed truth beside it")
	rosterHeaderInk = flag.Bool("roster.headerink", false,
		"column ink histogram over groupHeaderRegion — where a crop edge should go")
	rosterPower = flag.Bool("roster.power", false,
		"report what the power field reads per band, and whether ParsePower accepts it")
	rosterLevel = flag.Bool("roster.level", false,
		"report what the level field reads per band, and whether ParseLevel accepts it")
```

- [ ] **Step 2: Write the rank-badge mode — the highest-value instrument here**

This mode was added after the gate's first run, which found a defect no planned
probe would have measured. `res.PerGroup` came back with exactly **one** key:
every row in the capture was attributed to R3, including all of R2's. Five R2
members were created under R3 and the remaining six were never created at all,
because R3's creation budget (`expected: 64`) had been spent by 83 rows.

Rank does **not** come from header OCR. `roster.go:640` is
`groupKey := rankRes.Rank`, and that comes from `matchRankBadge` — hand-rolled
NCC against the embedded badge crops in `rankBadgeOrder` (`R1`–`R4`). So the
header count and the rank badge are two independent defects, and the badge is
the dominant one. Shipping this task's probes without an instrument for it
would repeat the exact mistake this milestone was founded on, on the very
defect the gate just exposed.

```go
// reportRosterBadge reports what matchRankBadge decides on every frame, beside
// what the fixture says that frame's group actually is.
//
// The gate found the whole of capture 1 attributed to R3 -- R2's rows
// included -- and no probe in this file could have seen that, because every
// other mode measures OCR and this decision is NCC. rankBadgeMinGap (0.20) is
// the constant in question and it has a documented history: it was raised from
// 0.15 after a reviewer produced a MEASURED near-miss at gap 0.162 that was
// accepted at the wrong rank. Half a distribution is what set it the first
// time; this is the other half.
func reportRosterBadge(t *testing.T, frames []rosterLoadedFrame, truth map[int]string) {
	t.Helper()
	agree, disagree, refused := 0, 0, 0
	for _, f := range frames {
		best, runnerUp, err := bestTwoRankScores(f.Img)
		want := truth[f.Seq]
		switch {
		case err != nil:
			refused++
			t.Logf("  seq %2d  want %-3s  REFUSED: %v", f.Seq, want, err)
		case best.rank != want:
			disagree++
			t.Logf("  seq %2d  want %-3s  GOT %-3s  best %.3f runner-up %s %.3f gap %.3f  <-- WRONG",
				f.Seq, want, best.rank, best.score, runnerUp.rank, runnerUp.score, best.score-runnerUp.score)
		default:
			agree++
			t.Logf("  seq %2d  want %-3s  ok       best %.3f runner-up %s %.3f gap %.3f",
				f.Seq, want, best.score, runnerUp.rank, runnerUp.score, best.score-runnerUp.score)
		}
	}
	t.Logf("  badge: %d agree, %d WRONG, %d refused, of %d frames", agree, disagree, refused, len(frames))
}
```

`truth` maps frame seq to the rank that frame's rows actually belong to. Derive
it from `fixtures/m4rostergate/expected.yaml` plus the frame list — or, if that
mapping is not recoverable from the fixture alone, read it off the frames and
say so in your report rather than guessing.

**Report the score distributions, not just the counts.** A wrong verdict at a
wide gap and a wrong verdict at a narrow one need opposite fixes: the first
says the templates match the wrong thing, the second says the threshold cannot
separate them. That distinction is what the next task acts on, and a bare
"N wrong" cannot express it.

- [ ] **Step 3: Write the header mode**

```go
// reportRosterHeader reads groupHeaderRegion on every frame and reports the
// raw text, what parseGroupHeader made of it, and why it refused.
//
// This is the field the route's dominant defect lives in: a header that will
// not parse makes roster.go `continue` before SegmentRows is ever called, so
// the whole group is dropped. Four of five groups produced nothing for exactly
// this reason, and the review queue's raw text names the cause -- "R2) I'm
// Alright VN iy]" keeps the group name and loses the N/M count, and the
// chevron sits inside groupHeaderRegion's right edge at X2=0.97.
func reportRosterHeader(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame) {
	t.Helper()
	ing := New(nil, nil, engine)

	ok, bad := 0, 0
	for _, f := range frames {
		res, err := ing.readField(ctx, f.Img, groupHeaderRegion, groupHeaderSpec, groupHeaderOptions)
		if err != nil {
			t.Logf("  seq %2d  READ ERROR %v", f.Seq, err)
			bad++
			continue
		}
		name, total, perr := parseGroupHeader(res.Text)
		if perr != nil {
			bad++
			t.Logf("  seq %2d  conf %.2f  %-40q  REFUSED: %v", f.Seq, res.Confidence, res.Text, perr)
			continue
		}
		ok++
		t.Logf("  seq %2d  conf %.2f  %-40q  -> %q total=%d", f.Seq, res.Confidence, res.Text, name, total)
	}
	t.Logf("  header: %d parsed, %d refused, of %d frames", ok, bad, len(frames))
}
```

- [ ] **Step 3: Write the header ink profile mode**

Copy `reportRosterInkProfile`'s structure but profile `groupHeaderRegion`'s own band across every frame rather than row bands, and print the full width (the header spans `0.03`–`0.97`, so the existing `0.10 < frac < 0.45` window would hide the right edge entirely). Mark the current `X2` in the output the way the existing profile marks `nameXFrac0`:

```go
		note := ""
		switch x {
		case int(groupHeaderRegion.X1 * float64(len(acc))):
			note = "  <- groupHeaderRegion.X1"
		case int(groupHeaderRegion.X2 * float64(len(acc))):
			note = "  <- groupHeaderRegion.X2"
		}
```

- [ ] **Step 4: Write the power and level modes**

Both walk `SegmentRows(f.Img, memberListRegion, memberRowPitch)` exactly as `readRosterNames` does, read their own field rect, and report raw text plus whether the parser accepted it. Report a summary line each: bands read, parser accepted, parser refused, empty.

Power's summary must also count how many refusals are **structurally one damaged separator** — a read matching `^Power.{0,2}\s*\d+.\d+M` — because that is the shape the review queue is full of (`Power:}175'1M`) and the number that will move when the crop moves.

- [ ] **Step 5: Wire them into `TestRosterNameProbe`**

```go
	if *rosterBadge {
		reportRosterBadge(t, frames, rosterFrameRanks(t))
	}
	if *rosterHeader {
		reportRosterHeader(ctx, t, engine, frames)
	}
	if *rosterHeaderInk {
		reportRosterHeaderInkProfile(t, frames)
	}
	if *rosterPower {
		reportRosterField(ctx, t, engine, frames, "power")
	}
	if *rosterLevel {
		reportRosterField(ctx, t, engine, frames, "level")
	}
```

- [ ] **Step 6: Run each mode and record the output**

```bash
make probe-roster PROBE_ARGS='-roster.header'     2>&1 | tail -80
make probe-roster PROBE_ARGS='-roster.headerink'  2>&1 | tail -60
make probe-roster PROBE_ARGS='-roster.power'      2>&1 | tail -40
make probe-roster PROBE_ARGS='-roster.level'      2>&1 | tail -40
```

All four must PASS — probes assert nothing. Save the output; it is the before-measurement Tasks 6 and 7 are read against.

**Treat implausible uniformity as a broken instrument, not a clean result.** If the header mode reports identical text on frames showing different groups, it is measuring nothing — this repo has already had a probe return 24 identical rows across shapes that cannot agree, and it read as a persuasive negative result while measuring a retry happening inside the engine before the probe saw it.

- [ ] **Step 7: Document the flags in the Makefile's `probe-roster` comment block, then commit**

```bash
go test ./...
git add internal/ingest/zz_roster_probe_test.go Makefile
git commit -m "Point instruments at the three roster fields that had none

The name field got a probe four commits ago and it immediately found a crop
edge sitting inside the per-member status icon, unmeasured for a milestone. The
group header, power and level have had nothing, and between them they account
for every fact the route fails to write: four of five groups dropped whole on a
header that will not parse, and zero power and zero level facts from 96 members.

The header mode reports raw text beside parseGroupHeader's verdict, so a
refusal names its own cause. The ink profile prints the full header width
rather than the name field's 0.10-0.45 window, because the suspect edge is
X2=0.97 and the existing window would hide it.

Power's summary counts refusals that are structurally one damaged separator --
'Power:}175'1M' against a frame that renders 'Power: 211.5M' -- because that is
the shape the review queue is full of and the number a crop change should move.

All four assert nothing. Reading the output is the point."
```

---

### Task 6: Move the group header crop off the chevron

The highest-yield fix in the plan: four groups, ~32 members.

**Files:**
- Modify: `internal/ingest/roster.go` — `groupHeaderRegion`, possibly a separate count rect and options
- Modify: `internal/ingest/parse_test.go` — real chevron-bleed strings
- Test: `internal/ingest/roster_test.go` — crop geometry

**Interfaces:**
- Consumes: the ink profile from Task 5.
- Produces: possibly `groupHeaderCountRegion transport.Rect` and `groupHeaderCountOptions vision.Options`, if measurement says the count needs its own crop.

- [ ] **Step 1: Pin the real failure strings first, without tesseract**

Add to `internal/ingest/parse_test.go`. These are verbatim from `review_queue` on the 2026-08-19 re-ingest, and they belong in `make test` so the parser's behaviour on real chevron bleed survives with no device, no Docker and no OCR:

```go
// The chevron-bleed corpus: what groupHeaderRegion actually handed
// parseGroupHeader on capture 1, taken verbatim from review_queue. Every one
// keeps the group name and loses the N/M count, because groupHeaderRegion's
// X2=0.97 right edge sits inside the collapse chevron.
//
// These are pinned as REFUSALS, not as things to be parsed. Relaxing the
// parser to accept them is the wrong fix and an actively dangerous one: task
// 24's review showed a fabricated count of 6 against a real 64-member group
// stops the other 58 members being created at all. The crop moves; the parser
// does not.
func TestParseGroupHeaderRefusesChevronBleed(t *testing.T) {
	for _, raw := range []string{
		"[R4) This Is It ap",
		"R2) I'm Alright VN iy}",
		"R2) I'm Alright Vu iy]",
		"R2) I'm Alright VN iy]",
		"iR2) I'm Alright Vn WY",
		"R2) I'm Alright VN iv]",
		"R2) I'm Alright VW iv]",
	} {
		if _, _, err := parseGroupHeader(raw); !errors.Is(err, ErrUnparseable) {
			t.Errorf("parseGroupHeader(%q) accepted a header with no count; want ErrUnparseable", raw)
		}
	}
}

// The other half of the same rule: a header that DOES carry its count parses,
// so the test above is pinning the absence of a count rather than a blanket
// refusal.
func TestParseGroupHeaderAcceptsARealCount(t *testing.T) {
	name, total, err := parseGroupHeader("{R3) Footloose 10/64")
	if err != nil {
		t.Fatalf("parseGroupHeader: %v", err)
	}
	if total != 64 {
		t.Errorf("total = %d, want 64", total)
	}
	if !strings.Contains(name, "Footloose") {
		t.Errorf("name = %q, want it to contain %q", name, "Footloose")
	}
}
```

- [ ] **Step 2: Run them**

```bash
go test ./internal/ingest/ -run TestParseGroupHeader -v
```

Expected: PASS. These document current behaviour rather than driving a change — the parser is not the defect. If either fails, stop: the diagnosis is wrong and the crop is not the whole story.

- [ ] **Step 3: Read the ink profile and choose the edge**

```bash
make probe-roster PROBE_ARGS='-roster.headerink' 2>&1 | tail -60
```

Find the gutter between the count's last digit and the chevron. **A gutter is a plateau, not a spike** — the name crop's shipped value sat on a three-column plateau (`x=156..158`, ink 16 against 787 and 946 on either side), and that flatness is what says the value is a gutter rather than a fit to noise. If the profile shows a spike and no plateau, say so and do not ship the value.

- [ ] **Step 4: Decide whether the count needs its own crop — by measurement**

Open question, deliberately not pre-answered: the count is cyan-and-white on light blue and the name is white, so one threshold serving both is a hypothesis. Measure both shapes with `-roster.header` and take the better:

- **(a)** move `groupHeaderRegion.X2` to the measured gutter, keep one crop;
- **(b)** add `groupHeaderCountRegion` spanning only the count, with its own `vision.Options`, and have `roster.go` read and combine the two.

Report both numbers in the commit message even though only one ships. `groupHeaderOptions`' existing comment records that every shape including adaptive-threshold-after-equalize scored 0-1/18 — **that was measured through the current crop, and options measured through the wrong rectangle are not evidence about the right one.** Re-measure; do not re-reason.

- [ ] **Step 5: Apply the change and re-measure**

```bash
make probe-roster PROBE_ARGS='-roster.header' 2>&1 | tail -40
```

The number to move is "refused". Record before and after.

- [ ] **Step 6: Pin the geometry with a device-free test**

Model it on `TestNameCropStartsInTheGutterRightOfTheStatusIcon`: assert the edge sits in the measured gutter, and **carry a guard that fails loudly if the fixture frame ever stops containing a chevron**, so the test cannot go quietly vacuous.

- [ ] **Step 7: Run the gate**

```bash
go test ./...
make gate-roster 2>&1 | tail -40
```

Coverage should rise substantially — this is the ~32-member fix. Record the new number.

- [ ] **Step 8: Commit**

```bash
git add internal/ingest/roster.go internal/ingest/parse_test.go internal/ingest/roster_test.go
git commit -m "Move the group header crop off the collapse chevron

<the before/after header refusal counts>
<the before/after gate coverage>

groupHeaderRegion spanned X1 0.03 -> X2 0.97, the entire header strip, and the
collapse chevron sits inside that right edge. Every failure kept the group name
and lost the N/M count -- 'R2) I'm Alright VN iy]' -- and roster.go continues on
a header-parse error before SegmentRows is ever called, so four of five rank
groups were dropped whole.

Third instance of one defect in this milestone: the status icon inside the name
crop, the chevron inside this one, and a leading '}' in the power crop. Each
time, a crop edge placed where a human reading the rectangle still sees the
right answer. Placed off an ink profile this time, on a plateau rather than a
spike.

The parser is untouched and the chevron-bleed strings are pinned as REFUSALS in
parse_test.go, without tesseract, so they survive in make test. Relaxing
parseGroupHeader would be the wrong fix and a dangerous one: task 24's review
showed a fabricated count of 6 against a real 64-member group stops the other 58
being created at all."
```

---

### Task 6b: Stop dropping a whole frame when one field fails

Inserted after Task 6, which established that R2's `1/11` count is unreadable by tesseract at any setting — the failure is the engine's classifier, not the crop — so R2's 11 members cannot be recovered by any crop or option change.

That is a fact about OCR. What follows is a fact about **our** code, and it is a defect on its own terms: `internal/ingest/roster.go` `continue`s on a header-parse failure, so **one unreadable field discards every row on that frame**. Twenty-two frames' worth of members currently vanish behind a single `unparseable_group_header` review row each, rather than one row per member. That is the silent drop this milestone exists to prevent, committed by the ingest path itself, and it would still be wrong even if every count read perfectly — a future capture with one smudged header would lose a whole group the same way.

**Files:**
- Modify: `internal/ingest/roster.go` — the header-failure path
- Test: `internal/ingest/roster_test.go`

**Interfaces:**
- Consumes: `parseGroupHeader`, `matchRankBadge`, `groupTracker{expected, matchedOrCreated}`, `run.queueReview`, `GroupTally`.
- Produces: no new exported surface.

- [ ] **Step 1: Write the failing test**

A capture whose header count cannot be parsed, but whose rank badge matches confidently, must still segment its rows, still attribute them to the badge's rank, and produce **one review row per unmatched row** rather than one per frame. Assert the per-row count, not merely that something was queued — the whole defect is the ratio.

- [ ] **Step 2: Run it and confirm it fails**

Expect one `unparseable_group_header` row and no per-row reviews.

- [ ] **Step 3: Decouple the two reads**

The count and the rank are already documented as independent reads with independent failure paths (`roster.go`'s own comment says so). Make the count's failure non-fatal to the frame:

- queue the `unparseable_group_header` review as now, then **continue processing the frame** rather than skipping it;
- take the rank from `matchRankBadge`, which is unaffected and measured correct at 61/61;
- create the group's tally with **no creation budget**.

**A group with no readable count does not create members.** It matches rows against members already known and queues every row that does not match. This is the point of the change and the line not to cross: the count is the structural guard against minting phantoms (`design §4`, "creation is gated on the group count, not on a confidence threshold alone"), so a group whose size is unknown has no budget to spend. Ending the silent drop must not become a licence to invent people.

Represent "no budget" explicitly — a sentinel or a separate flag — never as `expected: 0`, which `parseGroupHeader` already rejects as incoherent and which would read as a group of size zero.

- [ ] **Step 4: Run the test, then the full suites**

```bash
go test ./internal/ingest/ -run TestIngestRoster -v
go test ./...
```

- [ ] **Step 5: Mutation-check the budget guard**

Remove the no-budget condition and confirm a test goes red showing members created for a countless group. If nothing fails, the guard is unpinned and the change is one edit away from minting phantoms.

- [ ] **Step 6: Re-measure, and read the right number**

```bash
make gate-roster 2>&1 | tail -40
make gate-m4 2>&1 | tail -20
```

Coverage should **not** move: no members can be created for R2, so `covered` stays where Task 6 left it. What must move is **condition 3** — R2's rows become individually queued instead of vanishing — and **condition 4**, since R2 now has a tally.

**The number this task exists to produce** is how many R2 rows land in the review queue. That count is the measured value of building per-digit NCC for the header count, which is the open scope decision. Report it prominently and separately from the gate headline.

If coverage moves, that is a finding: it would mean members are being created without a budget.

- [ ] **Step 7: Commit**

Record in the message that this is a correctness fix independent of R2 — a frame is no longer discarded because one field on it failed — and that the count is still what gates creation.

---

### Task 7: The name residual, splits, and `last_active`

Whatever conditions 1, 2 and 3 still show after Task 6.

**Files:**
- Modify: `internal/ingest/roster.go`, and `internal/roster/*` only if the gate's own output justifies it

**Interfaces:**
- Consumes: the gate's condition output; `-roster.detail`, `-roster.retry`, `-roster.x0sweep`.

- [ ] **Step 1: Re-run the gate and read the three condition blocks**

```bash
make gate-roster 2>&1 | tee /tmp/roster-gate-after-header.txt
```

- [ ] **Step 2: Localize before changing anything**

For each missing member, use `make probe-roster PROBE_ARGS='-roster.detail'` to see what its band actually read. **An aggregate can be perfectly accurate and support the wrong story** — this milestone already concluded that 15 empty bands "were" the non-Latin names when per-member they were almost all plain ASCII, and no language pack would have moved one of them.

- [ ] **Step 3: Address orphans and splits by the mechanism the evidence names**

Do not pre-commit to a fix. Orphans are misreads that minted a person; splits are one person minted twice. If the evidence points at creation confidence, change that; if it points at the crop, measure and move the crop; if it points at normalization, change that. **Do not lower `roster.AutoAccept`** — it is what stops a misread row being attributed to the wrong member, and that is the one failure a review queue cannot undo.

- [ ] **Step 4: Measure `last_active`**

46 facts against 96 members, 10 unparseable and 13 low-confidence. The field renders as green `Online` or an elapsed time; the green is the first thing to measure, since a threshold fitted for white text on a light row has no reason to serve it.

- [ ] **Step 5: Check the separation budget after any matcher change**

If anything in `internal/roster` changed, `make probe-m4` prints `ClosestPairScore` and fails if it reaches `AutoAccept`. On the M4 capture it sits at 60 against a threshold of 92. Read every accuracy change against that number, not against the count alone.

```bash
make probe-m4 2>&1 | tail -20
make gate-m4 2>&1 | tail -20
```

**`make gate-m4` must not regress.** The VS route shares `internal/roster` with this one.

- [ ] **Step 6: Run everything and commit**

```bash
go test ./...
make gate-roster 2>&1 | tail -20
```

Commit with before/after numbers for every change, one commit per mechanism rather than one for the batch.

---

### Task 8: Repoint `probe-roster` at the ground truth, and retire the caveat

`probe-roster` scores against `fixtures/m4gate/expected.yaml`'s 86 VS names — incomplete (96 members) and three days apart — so its `exact` column is a lower bound, and that caveat currently dominates its doc comment, its Makefile block, `CLAUDE.md` and `README.md`. Once `expected.yaml` exists the caveat stops being true, and **a warning left standing after it stops being true is how the next person is misled.**

**Files:**
- Modify: `internal/ingest/zz_roster_probe_test.go` — `loadRosterTruth`, the doc comment
- Modify: `Makefile`, `CLAUDE.md`, `README.md`
- Delete: `fixtures/m4roster/frames.yaml`

- [ ] **Step 1: Point `loadRosterTruth` at the roster fixture**

Read names from `fixtures/m4rostergate/expected.yaml`, falling back to the VS fixture with a logged warning when it is absent, so a fresh clone still runs.

- [ ] **Step 2: Point `loadRosterFrames` at the same file**

`expected.yaml` carries the identical frame list. Delete `fixtures/m4roster/frames.yaml` so there is one fixture and no chance of the two drifting.

- [ ] **Step 3: Rewrite the caveats**

`exact` is now an accuracy. `junk-prefixed` remains a defect measure and stays. Update the probe's doc comment, the Makefile block, `CLAUDE.md`'s `make probe-roster` bullet (which says in those words to read `junk-prefixed` and never quote `exact`), and `README.md`'s M4 row.

- [ ] **Step 4: Run and commit**

```bash
make probe-roster 2>&1 | tail -20
go test ./...
git add -A
git commit -m "Score probe-roster against real ground truth, and retire its caveat

The probe scored names against the VS fixture's 86 scorers -- neither complete
(96 members) nor contemporaneous (three days apart) -- so its exact column was
a lower bound, and four separate places said so in those words. With a
transcribed roster it is an accuracy, and a warning left standing after it
stops being true is how the next person is misled.

fixtures/m4roster/frames.yaml is deleted rather than kept in step: expected.yaml
carries the identical frame list, and two copies of one list is a drift waiting
to happen."
```

---

### Task 9: Rule on the 1% tolerance, and close the milestone's documentation

**Files:**
- Modify: `internal/ingest/gate_test.go` — state the blind spot in the gate's own output
- Modify: `CLAUDE.md`, `README.md`, `fixtures/m4gate/README.md`

- [ ] **Step 1: Put the blind spot in the gate's output**

`gate-m4` counts a row correct within 1%. At rank 7 that is a window 183,000 wide, and `Mar 89` was written 18,356,304 against a hand-checked 18,356,804 and counted among the rows it passed. The ruling is that the bar **stays at 1%** — a tighter one would start failing rows for transcription ambiguity in `expected.yaml` itself, which is 86 numbers read by eye and not regenerable, and the alternatives have never been measured.

What changes is that the number cannot be quoted as more than it is. Extend the final `t.Logf`:

```go
	t.Logf("M4 gate: %d/%d rows within %.0f%% ... — a pass means each row reached the right member carrying roughly the right magnitude, NOT that the numbers are correct: at rank 1 this tolerance is a window ~180,000 wide and every low-order digit misread passes inside it",
		...)
```

- [ ] **Step 2: Update `CLAUDE.md`**

Add `make gate-roster` to the Quickstart and to the Testing section, with the same "what it can and cannot tell you" honesty the other entries carry. Record the chevron as the third instance of the crop-edge defect, in the section that already records the first two — the generalization is that **the same defect recurred in a third field, and the third time it was caught by an instrument rather than by a person noticing**, which is the argument for instrumenting a field before changing it.

- [ ] **Step 3: Update `README.md`'s M4 row**

Report the roster gate's number honestly beside the VS gate's, and say what each does and does not prove.

- [ ] **Step 4: Run the full suite and both gates**

```bash
go test ./...
make gate
make gate-m4
make gate-roster
```

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "Rule on gate-m4's tolerance, and document the roster gate

The 1% bar stays. A tighter one would start failing rows for transcription
ambiguity in expected.yaml itself -- 86 numbers read by eye, not regenerable --
and an absolute tolerance or one scaled to digit position are both plausible and
neither has been measured. Changing the bar is a decision about what the gate is
for, not a fix to make the current number better.

What changes is that the gate now says in its own output what a pass means: each
row reached the right member carrying roughly the right magnitude, not that the
number is correct. Rank 7 was written 18,356,304 against a hand-checked
18,356,804 and passed, because 500 on 18.3M is 0.0027%. The design doc has said
so since August; the gate had not."
```

---

## Self-Review

**Spec coverage.** §3 fixture → Tasks 2, 3. §4 four conditions → Task 4. §5.1 header → Task 6. §5.2 splits, §5.3 name, §5.4 `last_active` → Task 7. §5.5 power/level instrument-and-defer → Task 5 (probes) — note their *fixes* are deliberately absent, per the scoping decision. §6 instruments → Tasks 5, 8. §7.1 `vs.go:753` → Task 1. §7.2 tolerance ruling → Task 9. §8 testing → distributed. §9 sequence → task order.

**Known gap, stated rather than hidden:** the spec's §5.2 and §5.4 cannot be planned to the step, because what they fix depends on what the gate's first run shows. Task 7 is therefore procedural where every other task is concrete. That is a real limitation of writing this plan before the baseline exists, not an oversight — and the alternative, guessing a fix now, is what this repo's whole measurement discipline exists to prevent.

**Type consistency.** `expectedRoster`/`expectedGroup`/`expectedMember` and `loadExpectedRoster(t, path)` are defined in Task 2 and used unchanged in Tasks 3 and 4. `GroupTally.MatchedOrCreated` is added in Task 4 Step 1 and consumed in Task 4 Step 2. `gateRosterCoverage` and `gateRosterMinMembers` are declared in Task 4 Step 2 and Task 2 Step 1 respectively.

**Corrected during review — two errors that would not have compiled:**

- The matcher's entry point is `roster.Rank(raw string, members []Member) []Candidate`, sorted best-first, **not** `roster.Match`. `roster.Member` is `{ID, Name, Aliases}` and carries no `NameNormalized` — `TokenSetRatio` normalizes internally. Task 4's code is written against the real signature.
- **`join()`, `gatePool()` and `envOr()` live in `gate_test.go`, which is `//go:build m4gate`.** Building with `-tags m4rostergate` does not compile that file, so the roster gate cannot use them and must not redeclare them either (both tags together would collide). **Task 4 Step 3 must first extract those three helpers into `internal/ingest/gate_shared_test.go` guarded by `//go:build m4gate || m4rostergate`**, removing them from `gate_test.go`, and confirm `make gate-m4` still builds and runs before continuing.
