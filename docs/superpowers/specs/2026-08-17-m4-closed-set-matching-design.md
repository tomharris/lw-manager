# M4 — Closed-set matching: design

**Date:** 2026-08-17
**Status:** implemented on branch `m4-closed-set-matching`; §9 records what it
measured. Supersedes the "what is left" section of
`2026-08-17-m4-gate-name-matching-gap.md`; everything that document records
about *how* the current numbers were reached still stands.
**Gate:** unchanged — `make gate-m4` at ≥95% of rows within ±1%, **cold**.

Cold is a constraint, not a preference. The roster changes week to week, so a
design whose accuracy depends on a human clearing a review queue is a design
that degrades every time somebody joins the alliance. Aliases remain valuable
and remain in the pipeline; they are simply not allowed to be load-bearing.

---

## 1. Where the gate actually stands

`make gate-m4`, capture 6, game version 1.0.358:

    M4 gate: 65/86 rows within 1%, matched=71 queued=21 status=complete

The 21 queued rows split by reason, and the split is the whole design:

| reason | n | bucket |
|---|---|---|
| `no_confident_match` | 8 | name |
| `ambiguous_name_match` | 7 | name |
| `unparseable_points` | 4 | **points** |
| `low_confidence_points` | 2 | **points** |

**15 names and 6 points.** The points bucket was previously described as
"newly characterized"; it is now a third of the gap and cannot be closed by
anything on the name side.

## 2. The finding: the ranking is an assignment, not 86 lookups

`processRow` scores each row against all 86 members independently and accepts
at `roster.AutoAccept`. That discards three constraints the screen supplies for
free:

1. **Each member appears at most once.** The code half-knows this — the
   `scored` map deduplicates — but only in arrival order, after the fact. It
   never uses "B52RN10 is already pinned to its own row" as *evidence* when
   scoring some other row.
2. **Both sets are of known size and largely overlap.** The roster route has
   already established who is in the alliance.
3. **Rows are sorted descending by points**, so screen order is rank order.

Constraint 1 is what the name side needs; constraint 3 is what the points side
needs. Constraint 2 is what makes the residual small.

### Measured

A throwaway spike (`-probe.assign`) replayed `IngestVS`'s own band-to-row
arithmetic — `contentY + (band.Y0 - regionTop)` with the same geometric dedupe
— which produced exactly 86 deduped rows against 86 transcribed ones, and a
baseline of 71 correct, reproducing the gate's `matched=71` exactly. That
agreement is the reason the alignment can be trusted: two independent paths to
the same number.

| member set | matcher | correct | **wrong** | unassigned |
|---|---|---|---|---|
| square, PSM 7 | today (per-row ≥92) | 71/86 | 0 | 15 |
| square, PSM 7 | assignment, floor 60 / margin 20 | **83/86** | 0 | 3 |
| square, PSM 7+13 | today | 77/86 | 0 | 9 |
| square, PSM 7+13 | assignment, floor 60 / margin 20 | **84/86** | 0 | 2 |

The twelve rows recovered at PSM 7 are precisely the tail no threshold reached:

    rank  2  "Drizzlers12"       read "Drizzlers1z2"    ok (91)
    rank  3  "OrCasKillerBunny"  read "O'CsKillerBunny" ok (87)
    rank  6  "ZãP ꙅઉ"            read "ZaP oc"          ok (60)
    rank  7  "Mar 89"            read "Miar 号 9"        ok (66)
    rank 17  "AA91AA"            read "AASTIAA"         ok (80)
    rank 36  "Guts ツ"            read "Guts '\"         ok (80)
    rank 40  "Delio1"            read "beliot"          ok (66)
    rank 47  "Aureum ⊂6М"        read "Aureum CGU"      ok (75)
    rank 52  "OD15"              read "0015"            ok (70)
    rank 61  "Mc1999"            read "mais99"          ok (76)
    rank 66  "Syłar"             read "cular"           ok (60)
    rank 71  "ZeroOrca"          read "Zergoorca"       ok (88)

A read scoring 60 is meaningless against 86 candidates and decisive against the
four nobody has claimed. Three of the five decorated non-Latin names come back
this way, which is the class the previous document concluded was unreachable.

### The clean result was checked twice, because it was too clean

Zero wrong at every setting — including floor 0 / margin 0 — is exactly the
"implausible uniformity" CLAUDE.md warns about, and a forced assignment
returning a *perfect* result proved nothing about the instrument.

**Canary.** Rotating the truth labels by one rank gives `0 correct / 71 wrong`.
The counters do measure attribution.

**Squareness.** `expected.yaml` transcribes only the ranked rows and the probe
built its member list from those same rows, so every member had a row. Production
never looks like that: the weekly ranking lists scorers only, and recon measured
94 ranked rows against 96 alliance members. Padding the member set with 20
adversarial decoys — each one confusable substitution from a real name, e.g.
`Mcl999` against `Mc1999`, which is strictly harder than a real non-scorer:

| matcher | correct | **wrong** |
|---|---|---|
| today (per-row ≥92) | 76/86 | **1** |
| assignment, floor 60 / margin 20 | 81/86 | **1** |
| assignment, margin 0 | 84/86 | 2 |

Assignment adds five rows and introduces **no new misattribution**. The margin
requirement is what holds that line; at margin 0 a second wrong appears.

### A defect this exposed, unrelated to assignment

The one wrong row under decoys is **inherited from phase 1** — today's shipped
matcher produces it too. A decoy one confusable substitution from a real name
clears 92 outright, and `roster.ClosestPairScore` does not catch it because
`make probe-m4` calls it over the 86 *ranked* names rather than the full
alliance roster. The guard `confusable.go` describes as "the guard that makes
confusable scoring safe to tune" is currently scoped to a set that excludes
exactly the members most likely to break it. Fixing that is item 4 below and is
not an accuracy change.

---

## 3. Architecture

Two changes to `IngestVS`, one new pure package file, and two smaller items.

```
internal/roster/assign.go     NEW. Pure: score matrix -> assignment.
                              No OCR, no DB, no images. Device-free tests.
internal/ingest/vs.go         IngestVS becomes two-pass.
internal/ingest/points.go     NEW. Monotonicity bounds for the points field.
```

### IngestVS becomes two-pass

Assignment cannot be streamed: no row may be attributed until every row has
been read. So the frame walk splits.

```
pass 1  for each frame, for each band surviving geometric dedupe:
          read name field  -> text, per-member score vector
          read points field -> text, confidence
          hold {screenshotID, y0, y1, rank, scores, pointsText, pointsConf}
        no database writes at all

pass 2  assign rows to members (§4.1)
        resolve points against the assignment's rank order (§4.2)
        write facts / review rows / inferred zeros
```

This *improves* invariant #2 (idempotent and interruptible) rather than
straining it: today a crash midway through the frame walk leaves some facts
written and some not, and the two-pass shape means nothing is written until the
whole capture has been read. Memory cost is 86 rows of small structs against 21
decoded frames already held one at a time; it is not a consideration.

The identity cross-check (`matchedRowCount != len(scored)`) becomes true by
construction and is removed, replaced by the duplicate-row handling in §4.1.

`VSResult.Unidentified` keeps its current meaning — rows the pipeline could not
attribute — and so keeps gating the inferred zeros unchanged: a capture holding
rows it could not attribute has still proved nobody absent. Duplicates are
counted separately and explicitly do **not** count as unidentified, or a
capture containing the expected pinned self row could never infer a zero.

---

## 4. Design

### 4.1 Closed-set assignment

`roster.Assign(scores [][]int, opts) []int` — rows in, one member index (or
-1) per row out. Pure, total, and independent of where the scores came from.

Two phases, one rule:

> A row may claim a member only when that member is the best **free**
> candidate for that row, clears the phase's floor, and beats the next-best
> free candidate by the phase's margin.

- **Phase 1** — floor `AutoAccept` (92), margin 0. This is today's bar and
  pins the confident rows first.
- **Phase 2** — floor `ResidualFloor` (60), margin `ResidualMargin` (20),
  over whatever remains.

Each phase repeatedly claims the globally highest-scoring eligible pair, which
is a greedy approximation to a maximum-weight matching. It is deliberately not
Hungarian: greedy is `O(n³)` at n=86 (microseconds), reads in one screenful,
and — critically — cannot produce a *globally* optimal assignment that is
locally absurd. Optimal matching would happily displace a 100-scoring pin to
raise the total.

Phase 2 is **not a relaxed threshold**. It is a different criterion, and a
stricter one in the dimension that matters: it is conditioned on every
confident row already being pinned, so it cannot be satisfied by a read that
merely resembles a popular name. A member who has their own row is not
available to steal somebody else's.

**Duplicate rows.** The pinned self row appears twice in a capture by design
(recon §2). Under assignment the higher-scoring occurrence claims the member
and the other is left unassigned with its best candidate already taken at ≥92.
That is a duplicate, not a failure: it is dropped silently and counted in
`VSResult.Duplicates`, matching today's behaviour, and specifically **not**
queued — a review row per week for a structurally expected duplicate is noise
that trains a human to ignore the queue.

**Confidence.** This is the part that does not fall out of the existing code,
and getting it wrong silently erases the entire gain.

`writeFacts` computes `conf = min(matchNorm, fieldConf)` with
`matchNorm = score/100`, against `factConfidenceGate = 0.80`. A row assigned at
string-score 60 would therefore carry 0.60 and be queued anyway — the
assignment would resolve it and the fact writer would throw it away.

The fix is **not** to lower `factConfidenceGate`. `score/100` is simply the
wrong confidence model for an assignment: the score measures string similarity,
but the *claim being made* is "this member is the unambiguous winner among the
unclaimed, by a margin of ≥20, in a closed set where 71 other rows are already
pinned." That claim's strength does not vary with the string score.

So:

- Phase-1 matches keep `matchNorm = score/100`, exactly as today.
- Phase-2 matches take `matchNorm = residualMatchConfidence = 0.85`.

0.85 sits above `factConfidenceGate` and visibly below a clean match, so the
distinction survives into the fact and a human triaging later can see how a row
was resolved. It is a fitted constant like `confusableCost`, and the same rule
applies: re-measure, do not re-reason. `UpsertFact` only overwrites on strictly
higher confidence, so a later clean read of the same member supersedes a
residual match automatically, which is the correct direction.

### 4.2 Points: ordering-validated parsing

The ranking is sorted descending, so once §4.1 has fixed each row's rank, row
*i*'s points are bounded by its nearest confidently-parsed neighbours above and
below. That bound is a structural check of the same species as roster ingest's
"the group header states its own total" — a check where the alternative is a
tuned number.

Three uses, in increasing order of ambition:

1. **Reject.** A value that parses but violates its bounds goes to review. This
   is new safety, and it is what makes 2 and 3 defensible: the failure mode
   `vsPointsSpec`'s charset comment documents at length — a crop catching
   neighbouring content and manufacturing a plausible number — is exactly what
   an out-of-order value looks like.
2. **Corroborate.** A value that parses, satisfies its bounds, and carries OCR
   confidence below `factConfidenceGate` is no longer resting on OCR confidence
   alone. Two of the six failures are here and are **exactly right**:
   `8,835,180` at 0.52 (Handbol) and `1,242,375` at 0.70 (Nichoj).
3. **Retry.** An empty points read may retry at PSM 13, and the retry's value
   is accepted only if it parses *and* satisfies its bounds. This is the guard
   that answers the standing objection to a numeric retry: a manufactured value
   has no reason to land inside a narrow ordered window. Two of the six
   failures are empty reads.

A fourth use — **repairing** `¢,609,299` (albambet, want 2,609,299) and
`e,2¢8,001` (ZeL1, want 2,328,001) by solving for the misread character subject
to the bounds and the comma grouping — is specified as a separate, optional
task with its own measurement, accepted only when the bounds admit a **unique**
digit and rejected to review otherwise. It is listed last because it is the
only item here that constructs a value rather than validating one, and if items
1–3 plus §4.1 clear the gate it should be dropped rather than built.

**The points bucket will grow before it shrinks, and the plan assumes it.**
Every row §4.1 newly matches brings a points read that was never evaluated
before. At least one is known bad: the previous document measured `Mar 89` at
`18,356,304` against a true `18,356,804` — a genuinely wrong value the
confidence gate correctly rejects, and one that bounds will *not* rescue
because it is in order. Estimating this bucket at "6" would be reading today's
number as if the name side were not about to change underneath it.

### 4.3 PSM 7+13 union on the name field

Read each name crop at both modes unconditionally and take the per-member
maximum score. Worth +6 at today's baseline and +1 once §4.1 is in.

This needs its own justification because CLAUDE.md currently says a retry
gated on a low *match* score "would put the matcher upstream of OCR." That
objection is sound and does not apply here: both reads are produced
unconditionally, so nothing about the roster decides what OCR is asked to do.
Taking the better of two independent readings of the same pixels is the same
move the probe already makes across overlapping frames.

Cost is one extra tesseract invocation per row — roughly 27s to 53s over 86
rows, in a batch that runs daily.

Given the small marginal gain behind §4.1, this is **insurance rather than
accuracy**: it matters on a capture where assignment has less structure to work
with (a partial capture, a heavily-changed roster), which is precisely the case
no single fixture can measure.

### 4.4 `ClosestPairScore` over the whole roster

`make probe-m4` currently measures it over the ranked rows. Measure it over
every member instead, and additionally log a warning at ingest time when two
members on the roster score ≥`AutoAccept` against each other — that is a pair
the matcher cannot tell apart, which no threshold fixes and only an alias can.

Not an accuracy item. It closes the hole the decoy run exposed.

---

## 5. What is deliberately not done

- **Expanding `confusablePairs`.** Previously proposed as worth +2–3
  (`d↔b`, `c↔a`, `s↔c`, `d↔0`). §4.1 recovers `Syłar`, `Delio1`, `Mc1999` and
  `OD15` without it, and every added pair spends separation against exactly the
  near-neighbour case that produced the one wrong row under decoys. Dropped.
- **Discounting insertions and deletions.** `OrCasKillerBunny` (87),
  `ZeroOrca` (88) and `Drizzlers12` (91) are all pure indel misses and are
  agonisingly close. Measured: an indel cost of 8 tenths recovers only
  `Drizzlers12` (91→93) — `ZeroOrca` reaches just 91 and `OrCasKillerBunny`
  just 90 — for a discount applied to all 86 names. `confusable.go`'s existing
  argument stands, and §4.1 recovers all three anyway.
- **Lowering `AutoAccept`.** Unchanged and unchallenged.
- **Constraining the assignment by points order.** Rows are already in rank
  order and members have no intrinsic order, so monotonicity constrains the
  points field and not the name assignment. Kept to §4.2.
- **Aliases as an accuracy mechanism.** Still written on review resolution,
  still compounding, still not counted on.

---

## 6. Testing

- **`internal/roster/assign_test.go`** — the assignment is pure, so this is
  table-driven and device-free: square and non-square sets, a duplicate row, an
  ambiguous residual that must refuse, a cascade where a phase-1 pin displaces
  a later row, and the invariant that no member is ever assigned twice.
- **`-probe.assign` becomes a committed instrument**, replacing the spike. It
  keeps the canary (rotated truth) and the decoy padding, because both are what
  make its zeros mean anything, and both are the first things a future reader
  will want to re-run. Same tag and Makefile shape as `make probe-m4`.
- **`internal/ingest/points_test.go`** — bounds arithmetic against synthetic
  sequences, including the interesting cases: bounds that admit nothing, bounds
  that admit more than one repair, and a value in order but wrong.
- **`make gate-m4` is the acceptance test** and is not modified. If the
  fixture is touched at all this design is wrong — `expected.yaml` is 86 rows
  read by eye off 21 frames and is the one artifact that cannot be regenerated.
- `go test ./...` must still pass with no emulator, no adb, no Docker.

## 7. Risks

- **Every constant here is fitted to one capture.** `ResidualFloor 60`,
  `ResidualMargin 20`, `residualMatchConfidence 0.85`. The mitigation is the
  committed probe and the discipline that goes with it, not confidence in the
  numbers. Re-measure on the next capture before defending any of them.
- **Non-squareness is simulated, not observed.** The decoy run is a lower
  bound built from synthetic near-neighbours; the real non-scorers' names are
  not in any fixture. The first capture whose roster fixture includes
  non-scoring members should be measured before this is trusted at face value.
- **Cascade.** A wrong phase-1 pin displaces the correct member from another
  row. Measured at zero additional misattribution with margin ≥10, but it is a
  failure mode the per-row matcher does not have at all, and it is the reason
  phase 1 keeps margin 0 at a floor of 92 rather than being merged into one
  pass.
- **A stale roster.** A member absent from `members` cannot be assigned, and
  their row will compete for someone else. The floor and margin are what stop
  it; the ordering of ingest (roster before VS) is what makes it rare.

## 8. Open question

`residualMatchConfidence = 0.85` writes a fact for a row whose string evidence
may be as weak as 60. The structural evidence is strong and the decoy run
measured no misattribution, but this is the one place the design puts a number
on a leaderboard on the strength of an inference rather than a read. The
alternative — write the fact *and* queue a non-blocking confirmation row so the
alias mechanism still compounds — costs roughly a dozen queue rows per week and
was rejected as noise. Worth revisiting if the gate shows any residual-resolved
row producing a wrong value.

---

## 9. Results

> **Superseded in part by two later commits on this same branch.** Every number
> below stands as measured at `d1c6a35` and the reasoning is unchanged, but two
> of the three queued rows have since been resolved and the gate has moved.
> `479d941` measured the Greek and Arabic language packs against ranks 31 and 39
> and found them **inert** — both rows come back byte-identical under `+grc`,
> `+ara` and `+ell` — so this section's claim that they are "the missing
> language pack `CLAUDE.md` describes" is **wrong**, and `CLAUDE.md` has been
> corrected accordingly. `87163d2` then fixed both from the matcher instead
> (`roster.stripDecoration`). `make gate-m4` re-run 2026-08-19 reports
> **85/86 rows within 1%, matched=86 queued=1 zeroed=0, status complete** — the
> one remaining queued row is rank 77's repaired points, which is a points-stage
> failure and is described below.

Measured 2026-08-18 on branch `m4-closed-set-matching` at commit `d1c6a35`,
against **capture 6, game version 1.0.358, period `2026-W34`** — the same
21-frame hand-transcribed capture §1 was measured on, and the only one there
is. Every number below was read off a run made for this section; nothing is
carried forward from the tasks that produced it.

### The gate

    before (§1)  M4 gate: 65/86 rows within 1%, matched=71 queued=21 status=complete
    after        M4 gate: 83/86 rows within 1%, matched=84 queued=3 zeroed=0 status=complete (game version 1.0.358)

**83/86 = 96.51% against the 95% bar.** Conditions 2 and 3 pass: 3
discrepancies against 3 review rows, nothing dropped silently, and the capture
still reconciles to `complete`.

The "before" line is §1's, recorded before this branch existed and not re-run
here; the "after" line is `make gate-m4` in this session. The conditional
repair task (§4.2's fourth use) had already been built and measured by the
time this section was written, and no further work on it is triggered: the
gate passes with a one-row margin over the 82-row bar.

### The review queue, in full

Three rows, queried from `review_queue` scoped to the capture the gate seeded:

| rank | member | reason | raw text |
|---|---|---|---|
| 31 | `ϟϟ Leo ϟϟ` | `no_confident_match` | `soleoss` |
| 39 | `٣١٢ A l i ٣١٢` | `no_confident_match` | `wali` |
| 77 | `albambet` | `low_confidence_points` | `¢,609,299` |

Two name-stage failures and one points-stage failure. 31 and 39 are the
decorated-glyph names an English-only tesseract cannot read at any setting.
`make probe-m4 PROBE_ARGS=-probe.detail` scores their best read against their
own member at **28** (`ϟϟ Leo ϟϟ`, best read `">> Lea >>"`) and **22**
(`٣١٢ A l i ٣١٢`, whose best band was won outright by another member at 100) —
both far below the residual floor of 60, so no threshold and no margin reaches
them. That is the missing language pack `CLAUDE.md` describes under "OCR reads
the glyph, not the codepoint", not a tuning problem, and closed-set matching
was never going to touch it. 77's name matches; its points read `¢,609,299`,
which parses only once `repairPoints` solves the damaged leading position, and
a repaired value ships un-promoted by design — a constructed value has no pixel
evidence behind the position that was solved for — so the row queues rather
than being written. All three are recoverable queue rows; none is a
misattribution.

### `make probe-assign` at the shipped floor and margin

86 deduped rows, 86 members, reads at PSM 7:

| matcher | correct | wrong | unassigned |
|---|---|---|---|
| baseline (per-row, ≥ `AutoAccept` 92) | 71/86 | 0 | 15 |
| assignment, floor 60 / margin 20 (shipped) | **83/86** | **0** | 3 |

The grid around the shipped setting, which is what a future change is read
against — floor 60 is flat from margin 20 down to 0 at 83/86, and margin 30
costs four rows by refusing them:

    floor  margin   correct  wrong  unassigned
    65     20       81/86     0      5
    60     30       79/86     0      7
    60     20       83/86     0      3
    60     10       83/86     0      3
    40     10       84/86     0      2
    0      10       86/86     0      0

`duplicates: 0` — capture 6 contains no second sighting of the pinned self row,
so the between-phases duplicate guard is unexercised here and remains a
production-only safeguard.

### The two self-checks that make those zeros mean anything

**Canary (`-probe.assignshuffle`).** Truth labels rotated by one rank, so every
assignment is wrong by construction. Baseline `0/86 correct, 71 wrong`;
assignment at floor 60 / margin 20 `0/86 correct, 83 wrong`. 0 correct in every
one of the 35 grid cells. The counters do measure attribution.

**Decoys (`-probe.assigndecoys=20`).** 20 adversarial decoys, each one
confusable substitution from a real name (`Mcl999` against `Mc1999`, `Leroy
Jenkins o914` against `Leroy Jenkins 0914`), giving 106 members against 86 rows:

| matcher | correct | wrong |
|---|---|---|
| baseline (per-row, ≥92) | 70/86 | **1** |
| assignment, floor 60 / margin 20 | 77/86 | **1** |
| assignment, floor 0 / margin 0 | 84/86 | **2** |

Assignment adds seven rows and introduces **no new misattribution**: the one
wrong row is inherited from phase 1 and today's shipped per-row matcher
produces it too. The margin is what holds that line — at floor 0 / margin 0 a
second wrong appears. Note the shape of the cost: 77/86 under decoys against
83/86 square, so roughly six of the residual recoveries depend on the member
pool being exactly the rows on screen. That is the number to re-measure first
on the first capture whose roster fixture contains real non-scorers.

### The two field probes

    make probe-m4     psm7 shipped   73/86 distinct   120/142 bands   0 empty
    make probe-points psm7 shipped   83/86 rows exact 135/142 bands exact
                                     136 within 1%  6 unparseable  0 empty
                                     7 low-conf  4 retried

`roster.ClosestPairScore` over the 86 ranked rows is **60** — `"ALBAN80"` vs
`"albambet"` — against `AutoAccept` 92, a margin of 32. That is the budget any
change to `confusableCost` or the pair table is read against, and it has not
moved.

The points probe's three rows that never read cleanly are ranks 20, 79 and 77,
which is a *different* set from the gate's three misses (31, 39, 77). The
probe scores the best band per rank and attributes bands to ranks by value, so
it reports what the capture contains; the gate reports what one deduped
sighting per row produced. Reading either as the other is a mistake, and the
next section is what it costs.

### Rank 7: the gate's 1% tolerance hides a wrong number

Rank 7 `Mar 89` was written as **18,356,304** against the hand-checked
**18,356,804** — a single digit, an 8 read as a 3. This is the most important
thing the milestone produced and it contradicts what an earlier draft of
`vs.go`'s promotion comment claimed, which was that this capture contains no
real wrong promoted value at any window width.

Verified here by dumping all 83 facts the gate wrote and diffing them against
`expected.yaml` member by member. It is the **only** value-level disagreement
among the 83; the other 82 are exact.

- **The window cannot see it.** Its neighbours' written facts are 17,219,876
  (rank 8) and 19,247,540 (rank 6), so the window is 2,027,664 wide against a
  value of 18,356,304 — ratio **0.1105**, computed here from the written
  facts. Against the promoted population's ratios as instrumented when the
  width check landed (0.0348 / 0.0540 / 0.0550 / 0.1105 / 0.1508 / 0.3134 /
  0.5114, seven rows, and recorded in `vs.go`'s promotion comment rather than
  re-measured for this section) that is the *second narrowest* window of the
  seven. Both the right value and the wrong one sit comfortably inside it.
  No width threshold could separate them, and tightening toward 0.1105 would
  discard five correct rows before reaching it. The width check's scope is
  wrong-*magnitude* values only; a low-order digit error is invisible to
  ordering by construction.
- **The confidence cannot see it either.** The read's own OCR confidence is
  **0.6380**, below `factConfidenceGate` — so absent promotion it would have
  queued for a human, and promotion wrote it at 0.80 instead. That is the
  trade the width check makes, stated at full price. A confidence floor would
  not have saved it: 0.638 is well *above* the two lowest correct promotions
  in this population, which `vs.go`'s promotion comment records at 0.0853 and
  0.3383 — so any floor excluding rank 7 would have excluded those first.
  Confidence and correctness are not ordered together here.
- **The gate cannot see it.** 500 on 18.3M is 0.0027%, inside `gateTolerance`
  of 1%, so rank 7 is counted among the 83 rows that pass. The gate's own
  report cannot surface this by construction.
- **The capture contains the right answer.** Reading every band of every frame
  finds two sightings of this row: frame 0 at y0=1070 reading `18,356,304` at
  confidence 0.6380, and frame 1 at y0=729 reading `18,356,804` at confidence
  **0.8531** — which would have cleared `factConfidenceGate` on its own with no
  promotion at all. The geometric dedupe kept the frame-0 sighting. So this
  particular row is not a limit of the OCR; it is a limit of choosing one
  sighting per row and never comparing it with the others. Nothing in the
  current design does that comparison, and nothing here proposes it — but it
  is the first place to look if low-order digit accuracy ever becomes the
  binding constraint.

The general form of this is written down in `CLAUDE.md` as "A passing aggregate
hides everything its tolerance is wider than". The operational consequence for
this project: **83/86 means each row reached the right member carrying roughly
the right magnitude. It does not mean the numbers are correct**, and it must
not be quoted as if it did.

### Open questions

**Is a 1% relative tolerance the right bar for this gate?** Rank 7 shows what
it costs — at the top of the ranking, 1% is a window 183,000 wide, and every
low-order digit misread passes silently inside it. A tighter tolerance would
have caught rank 7; it would also start failing rows for transcription
ambiguity in `expected.yaml` itself, which is 86 numbers read by eye off
screenshots and is not regenerable. An absolute tolerance, or a tolerance
scaled to digit position rather than to magnitude, are both plausible and
neither has been measured. This section deliberately does not answer it: the
blind spot is now recorded, and changing the bar is a decision about what the
gate is for, not a fix to make the current number better.

**Does the residual phase hold up off this capture?** §7's first risk is
unchanged — `ResidualFloor 60`, `ResidualMargin 20` and
`residualMatchConfidence 0.85` are all fitted to capture 6. The decoy run is a
synthetic lower bound and the 77/86-under-decoys reading above is the honest
figure to hold in mind, not the 83/86.

**Is a real wrong promoted value ever wrong by magnitude?** The width check at
1.0 was fitted against real-correct rows on one side and *synthetic* wrong rows
on the other; no real wrong-magnitude promoted value has ever been observed. The
first one must be used to refit against what the check excludes, not to
re-confirm what it admits.
