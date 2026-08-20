# M4 — The roster gate: design

**Date:** 2026-08-20
**Status:** approved, not yet implemented.
**Gate:** new. `make gate-roster` at ≥95% member coverage with **zero splits**,
against a hand-transcribed roster capture.

This closes the half of M4 that no instrument was ever pointed at. The VS route
has a gate (`make gate-m4`, 85/86 as of 2026-08-19) and three probes. The
roster route has one probe, added four commits ago, and no gate at all — and
the milestone has now twice discovered that the unmeasured thing is the thing
that is wrong.

---

## 1. Where the roster route actually stands

Measured against capture 1 as re-ingested on 2026-08-19, read out of the dev
database rather than from any summary line:

| field | facts written | review rows |
|---|---|---|
| name → `members` | **57** of 96 | 13 `low_confidence_name` |
| `last_active_hours` | **46** | 10 unparseable, 13 low-confidence |
| `power` | **0** | 83 `unparseable_power` |
| `level` | **0** | 48 unparseable, 35 low-confidence |
| group headers | — | 22 `unparseable_group_header` |

Three things that table does not say, and that only a per-item view found.

**Every one of the 57 members is `R3`.** `roster.go` `continue`s on a
header-parse error before `SegmentRows` is ever called, so a group whose header
will not parse is dropped whole, and `R3 Footloose` is the only group that
produced anything. The name field was never the dominant defect.

### What capture 1 actually contains, read off the frames

M4's design §6 records this alliance as `R5 1 + R4 9 + R3 64 + R2 11 + R1 11 =
96`. **Both of those numbers are wrong**, and the correction changes what this
gate can measure, so it is recorded here rather than left to be rediscovered.

The alliance frame reads `Members: 97/100`, `[OrCa] Organized Chaos`, leader
`RobElr`. The four rank-group headers read `R4 This Is It 9`, `R3 Footloose
64`, `R2 I'm Alright 11`, `R1 Danger Zone 12` — 96 — and the leader occupies
the screen's banner rather than any group's list, which is the 97th. R5 is not
a rank group at all; `rankBadgeOrder` covering `R1`–`R4` is correct, not a gap.

**Only two of the four groups were ever expanded.** `R3 Footloose` and `R2 I'm
Alright` carry the down chevron and are each followed by exactly as many rows
as their header states — 64 and 11, counted. `R4 This Is It` and `R1 Danger
Zone` carry the **up** chevron and are followed by no rows at all, anywhere in
the capture's 61 member-list frames. `roster_capture` opens whichever
`chevron_collapsed` anchor `Match` finds next and stops once none remain; this
run ended with two still closed. The leader, separately, has no rank-group row
at all — only the banner.

So **75 members are reachable** — R3's 64 and R2's 11 — and the route's honest
standing is **57 of 75 (76%)**, not 57 of 97. A gate whose denominator were the
alliance count would be unreachable by construction at any bar above 77.3%,
which is a property of the capture and not of the pipeline.

This correction was itself an instance of the defect this project keeps
rediscovering. An earlier pass read `seq 1`, saw the header `R4 This Is It 2/9`,
and recorded R4 as expanded **without checking whether any rows followed it** —
R3's header comes immediately after, with nothing between. The chevron's
direction was the evidence and it was never consulted. Reading a frame and
seeing what you expected is exactly the eye-check failure documented twice
already for crops; it works the same way on a screen.

**The header failures are a crop defect, and the raw text names it:**

    [R4) This Is It ap
    R2) I'm Alright VN iy}
    R2) I'm Alright Vn WY
    R2) I'm Alright VN iv]

The group name survives; the `N/M` count does not. `groupHeaderRegion` spans
`X1 0.03 → X2 0.97` — the entire header strip, read as one field at one PSM with
grayscale and upscale(3) and no thresholding — and the collapse chevron sits
inside that right edge. `VN iy]` is the chevron.

**Power is not a legibility problem.** The frames render `Power: 211.5M`
cleanly. What OCR returns is

    Power:}175‘1M    Power}155:9M)    Power[2419M,    Powerte327M

— a leading `}`/`[`/`t`, a decimal point read as `'`/`:`/`°`/`<`/`,`, and a
trailing `)`/`,`. `ParsePower` refuses all of it, which is **correct**: task 23
removed a charset whitelist that had been laundering 33/53 of these into
well-formed wrong values 10x–1000x off. The pipeline is failing safe and
yielding nothing.

### The same defect, three times

The status icon inside the name crop, the chevron inside the header crop, and
whatever produces `}` in the power crop are one defect wearing three hats: a
crop edge placed where a human reading the rectangle still sees the right
answer. `CLAUDE.md` already records two instances and the method that finds
them. This design's premise is that the method — an ink profile over every
available band, not a read-back by eye — is now applied to the remaining
fields, and that each field gets a committed instrument so the next occurrence
is caught by a number instead of by someone noticing.

---

## 2. Scope

**In the gate's bar: name and `last_active`.** These are what M5 consumes — the
leaderboard needs member identity, the inactivity watchlist needs last-active —
and M5's gate is "one full week of real alliance data producing a leaderboard
you would actually post in alliance chat." A leaderboard missing a third of the
alliance is not one.

**Instrumented and measured, then deferred: power and level.** Both are at 0%.
Both get a probe and a recorded measurement in this milestone, and neither is
required to yield facts for M4 to close. The measurement is the deliverable;
recording it is what stops the next person re-reasoning the fix from the
review-queue text.

**Out of scope:** the M5 surface itself, any second capture, and the
`alliance_duel`/`gift_chest`/`tech_donation`/`rally_log` routes that M4 §1
already excluded.

---

## 3. The fixture

`fixtures/m4rostergate/expected.yaml`, with a README on the model of
`fixtures/m4gate/README.md`.

### What it records

Per member: rank group, name, power, level, and online-or-last-active. **All
four fields, though only two are in the bar** — transcription is the expensive
part of this design and doing it twice would be indefensible. The gate reads
the two in scope; the other two feed the probes.

Per group: the rank badge, the group name, the header count's `M`, and whether
the group was **expanded** in this capture. Group counts are the reconciliation
ground truth and are currently the dominant defect, so they are ground truth
here rather than something the gate infers. The `expanded` flag is what lets
`R1 Danger Zone 0/12` be recorded as a group the capture saw and never opened,
rather than as 12 members the pipeline lost.

Group totals sum to 96 against an alliance count of 97, and the difference is
the leader, who occupies the banner and has no rank-group row while every other
member has one. The loader asserts exactly that relation rather than an
equality, and a future alliance where it does not hold fails loudly — which is
the right failure, because it means the screen's structure changed.

Plus the provenance block the VS fixture carries — capture id, period key,
game version, alliance tag and name, and the frame list as `(seq, sha256,
offset_px)`. `offset_px` is copied from `capture_frames`, never re-derived: it
was measured against the frames as captured and ingest turns it into row
positions.

### How it is produced

Read off the full-resolution frames, one at a time, out of the blob store.
Not from `control ingest`'s summary, not from a `participation_facts` query,
not from a previous gate run — the reasoning in `fixtures/m4gate/README.md`
applies here unchanged, and this project has already paid once for a label that
had nothing to disagree with it.

**The transcription rule: a value is recorded only if it reads identically in
every frame it appears in.** Capture 1 carries ~3.8x overlap (331 row bands
across 61 member-list frames for ~96 members), so that is a real check rather
than a formality. Disagreements and genuinely ambiguous glyphs are marked in
the file, not guessed — the way the VS fixture marks its thirteen decorated
names as "a best reading rather than a certain one."

### On who transcribes it

`fixtures/m4gate/README.md` calls `expected.yaml` "the one artifact in this
repo that cannot be generated." That claim is about *provenance*, not about
species: what makes it ground truth is that it is read off the pixels through a
path that shares nothing with the pipeline it judges.

The eye-check that failed twice in this milestone was a check of **a crop** — a
rectangle whose contents the reader already knew, which is exactly why
`[icon fragment]GersonGamer` read as a confirmation. Transcribing a **full
frame** is the opposite act: the screen as the game rendered it, with no crop
to be fooled by, at full resolution, through vision rather than through
tesseract-on-a-preprocessed-band. The cross-frame agreement rule is the
additional guard, and it is stronger than what the VS fixture had.

This is recorded here because it is a judgement call, and a future reader is
entitled to know it was made deliberately rather than by default.

---

## 4. The gate

`make gate-roster`, tag `m4rostergate`, following every convention `gate-m4`
established: device-free, explicit `-timeout`, `LW_BLOB_FS_ROOT` defaulted with
`?=` in the Makefile, and a **named skip** when the fixture is absent or a frame
is missing from the blob store rather than a failure that reads as though the
numbers were wrong.

### Four conditions

**1. Coverage — ≥95% of transcribed members exist in `members`, attributed to
the right rank group.** The 95% is taken from `gate-m4`'s bar rather than
derived from what the pipeline currently does. That ordering is deliberate: the
VS gate's 95% came from the design doc and the pipeline had to climb 63/86 →
85/86 to reach it. A bar set after seeing the number is a bar fitted to the
pipeline.

**The denominator is transcribed members (75), not the alliance count (97).**
Those differ by R4's collapsed 9, R1's collapsed 12 and the leader's banner
row, and none of them is anything the pipeline could read from these pixels — scoring against 97 would
be scoring against frames the capture does not contain. The cost of the
narrower denominator is stated plainly: **R4, R1 and the leader are never
exercised by this gate**, so a defect specific to a collapsed group or to the
banner would go uncaught, and only a capture that expands R1 can close that.
The fixture therefore transcribes **every group's header** — R4's and R1's
included, at their true totals — even though it transcribes no members for
either, so condition 4 still measures the reconciliation of a group the route
saw and never opened.

**2. Zero splits.** Correspondence between a `members` row and a transcribed
member is judged by `roster.Match` against the transcribed set at `AutoAccept`.
Then:

- every `members` row must correspond to some transcribed member; and
- **no two `members` rows may correspond to the same transcribed member.**

The second clause is the one with teeth, and the distinction matters. A
cosmetically wrong display name is recoverable: `ALBANSO` for a real `ALBAN80`
is a documented confusable, so when VS ingest later reads the name correctly,
`roster.Match`'s confusable scoring matches it to that same row and the facts
land in one place under a slightly wrong label. What is **not** recoverable is
the same person minted twice under two different reads, because their facts
then split across two rows and no review-queue resolution rejoins them. That is
the roster route's equivalent of a VS misattribution, and like it, it gets a
hard zero rather than a percentage.

`gate-m4` has no analogue for this condition because it cannot happen there:
the VS route matches into a closed set and the roster route *creates*.

**3. Nothing dropped silently.** Every transcribed member that was never
created produced a `review_queue` row under a **name-class** reason —
`unreadable_name`, `ambiguous_name_match`, `low_confidence_name` or
`no_confident_match_group_full`.

Both halves of that sentence are load-bearing and the first draft got both
wrong. It counted **every** pending review for the capture, and `IngestRoster`
queues up to three *field-level* reviews (`unparseable_power`,
`low_confidence_level`, …) for each row that matched **successfully** — so 224
reviews against ~120 rows was almost entirely traffic with no relationship to a
missing member, and the condition could not fail. And it counted wrong-group
members among the missing, which is unaccountable in the other direction: a
misattributed row writes its facts and queues no name review at all, by
construction.

Scoped correctly, the condition went red on the first capture it was pointed
at: 25 members never created against 13 name-class reviews, so **12 members
were lost with nothing queued for them under any name reason**. That is the
silent drop this milestone exists to prevent, and it read green until the
counting rule was fixed.

**4. Reconciliation reports truthfully.** Not "the capture reaches `complete`",
but, concretely:

- for every group in the fixture, `RosterResult.PerGroup[rank].Expected` equals
  the group's transcribed header total;
- a group that produced **no tally at all** is reported, unconditionally;
- `Status` is `partial` if either a group fell short of its own expected count
  **or** the alliance-total check failed — both halves of `IngestRoster`'s real
  rule, not just the first.

So a `partial` capture that correctly reports `R2: 0 of 11` **passes**; one
claiming `complete` while a group is missing **fails**, and so does one whose
`Expected` disagrees with the transcribed header.

**What this condition deliberately does not assert, and why.** The first draft
also compared each group's `MatchedOrCreated` against the number of that
group's transcribed members found in `members`. It looked like a reconciliation
check and was not one. `MatchedOrCreated` counts **row events** — including a
row that re-matched a member created earlier in the same run, and a row that
minted an orphan — while the fixture knows only **members**. On the first
baseline R3 reported 83 against 45 transcribed members found, and the 38-row
gap decomposed exactly as 26 duplicate re-matches + 7 orphans + 5 R2 members
created under R3. Reconciliation had told the truth; the gate printed
`reconciliation does not describe what was parsed` and named the wrong culprit.

That is this repo's own "two aggregates side by side are not a causal claim",
committed by the gate built to enforce it. The check was deleted rather than
rescoped, because the gate has no independent row count to compare against and
a check that can only be green once conditions 1 and 2 already are adds no
signal. The attribution failure it was groping for is reported by condition 1,
which names the group a member was actually created in.

### What the gate will do on its first run

Fail. Conditions 1, 2 and 4 all fail against today's pipeline — 57 of 96, at
least two members whose correspondence needs checking, and four groups missing
entirely. **That red baseline is recorded before any fix**, because a gate whose
first run is green was fitted to the pipeline it was supposed to judge.

---

## 5. The fixes, in yield order

**5.1 The group header count — worth ~32 members.** Ink profile over the header
band across every frame in the capture, place the right edge in a gutter inside
the chevron, and re-measure. Whether the count needs its **own crop and its own
preprocessing** rather than sharing the name's is left open here deliberately:
the count is cyan-and-white on light blue and the name is white, so one
threshold serving both is a hypothesis. It gets measured, not reasoned.

`parseGroupHeader` itself is not the defect and is not being relaxed. Its
`total <= 0 || shown > total` check is what stops a fabricated count, and task
24's review showed exactly what fabrication costs: a phantom `6` against a real
64-member group stops the other 58 from being created.

**5.2 Splits.** Whatever condition 2 finds. Deliberately not pre-solved — the
mechanism depends on whether real splits exist in the capture at all, which the
gate's first run answers.

**5.3 The name residual — 57 of 64 inside R3**, 13 `low_confidence_name`. This
may already be near its floor after PR #12's crop fix; it cannot be known until
it is measured against complete ground truth instead of the VS fixture's
incomplete, non-contemporaneous 86 names.

**5.4 `last_active`** — 46 facts, 10 unparseable, 13 low-confidence. The field
is green `Online` or a time string; the green is the first thing to measure.

**5.5 Power and level** — instrument, measure, record, defer.

---

## 6. Instruments

`zz_roster_probe_test.go` gains modes rather than sprouting sibling files,
since they share a fixture and a harness:

- `-roster.header` — what the header band reads, per frame, with the parsed
  `N/M` beside it and the transcribed truth beside that.
- `-roster.headerinkprofile` — the column histogram the new right edge is
  placed from, mirroring `-roster.inkprofile`.
- `-roster.power`, `-roster.level` — the deferred fields, so their state is a
  recorded number rather than an impression.

All assert nothing and always pass. Reading the output is the point.

**Once `expected.yaml` exists, `probe-roster` should score against it rather
than against the VS fixture's 86 names** — at which point its `exact` column
stops being a lower bound and becomes an accuracy, and the caveat that
currently dominates its doc comment, its Makefile target and `CLAUDE.md` can be
retired. That retirement is part of this work, not a follow-up: a warning left
standing after it stops being true is how the next person is misled.

---

## 7. Two items independent of the roster

**7.1 `internal/ingest/vs.go:753` — zero rows on a `complete` capture zero every
member.** The guard is `capture.Status == "complete" && run.res.Unidentified == 0`,
and `Unidentified == 0` holds **vacuously** when nothing parsed at all. A capture
that produced no rows and was nonetheless marked complete writes an inferred
zero at confidence 0.90 for every member on the roster — a confident number on
a leaderboard derived from no read whatsoever, which is what invariant #5
exists to forbid. It also poisons the correction path: `UpsertFact` only
overwrites on strictly higher confidence, so those zeros outrank the real reads
a later ingest produces.

Present on `main`, neither introduced nor worsened by the closed-set branch,
and recorded in that branch's final review as I6.

Fix: require that the run actually parsed rows before inferring absence. Test
asserts no zeros are written for a complete capture with zero parsed rows, and
is **mutation-checked** — the guard is removed and the test confirmed red —
before the fix lands.

**7.2 Rule on `gate-m4`'s 1% tolerance.** At rank 7 of the M4 capture, 1% is a
window 183,000 wide; `Mar 89` was written 18,356,304 against a hand-checked
18,356,804 and counted among the rows the gate passed. Every low-order digit
misread in that capture passes the same way.

This design does **not** change the tolerance. It records the ruling: the bar
stays at 1% for M4, because a tighter one would start failing rows for
transcription ambiguity in `expected.yaml` itself — 86 numbers read by eye and
not regenerable — and the alternatives (an absolute tolerance, or one scaled to
digit position) have never been measured. What changes is that the blind spot
is stated in the gate's own output rather than only in a design document, so
the number cannot be quoted as more than it is.

---

## 8. Testing

| layer | tag | covers |
|---|---|---|
| unit | none | `parseGroupHeader` on real chevron-bleed strings; crop geometry; the `vs.go` zero-inference guard |
| gate | `m4rostergate` | ingest vs the hand-transcribed roster capture |
| probe | `m4probe` | header, power, level modes — assert nothing |

The real failure strings (`R2) I'm Alright VN iy]` and its variants) go into
`parse_test.go` as fixtures, so the parser's behaviour on real chevron bleed is
pinned **without tesseract** and survives in `make test`.

Crop geometry gets the treatment `TestNameCropStartsInTheGutterRightOfTheStatusIcon`
established: pin the edge to the measured gutter, and carry a guard that fails
loudly if the fixture frame ever stops containing a chevron. A test that cannot
fail is worse than no test.

`go test ./...` must still pass with no emulator, no adb and no Docker.

---

## 9. Sequence

1. `vs.go:753` — small, independent, live correctness bug.
2. Transcribe capture 1. Long pole; everything downstream needs it.
3. Build the gate; run it; **record the red baseline**.
4. Probes: header, `last_active`, power, level.
5. Fixes in §5's order, re-measuring after each.
6. Point `probe-roster` at `expected.yaml`; retire the lower-bound caveat.
7. Write the tolerance ruling into the gate's output and `CLAUDE.md`.

---

## 10. Risks

**The 95% bar may be unreachable on capture 1.** Four groups are missing
entirely and the header fix is unproven. If the bar cannot be reached, the
honest response is to record the measured number and say so — not to lower the
bar to meet it. `CLAUDE.md`'s standing rule about `AutoAccept` applies to gate
bars for the same reason.

**Capture 1 predates the capture-side interleaving fix.** The gate will bake in
a capture shape that new captures will not have, so a future clean capture may
exercise paths this gate never touches. Accepted deliberately, to start work
without a handset session; the second-capture check remains outstanding for
this route exactly as §9 of the closed-set design records it outstanding for
the VS route.

**Transcription error is correlated-failure risk.** If a name is genuinely
ambiguous at full resolution, my reading and the pipeline's could be wrong the
same way, and the gate would confirm rather than catch it. The cross-frame
agreement rule and explicit marking of ambiguous glyphs are the mitigations;
neither is a proof, and any row the gate fails on should be re-read against the
frames before the pipeline is blamed.

**Every measurement still rests on one capture per route.** `ResidualFloor`,
`ResidualMargin` and `residualMatchConfidence` are fitted to capture 6; whatever
the header fix settles on will be fitted to capture 1. The honest figure to hold
in mind is the decoy-padded one, not the headline.
