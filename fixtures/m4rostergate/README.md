# The roster gate's hand-transcribed capture

`make gate-roster` runs ingest over one real roster capture and checks the
members it creates against `expected.yaml` in this directory. That file is the
ground truth and, like `fixtures/m4gate/expected.yaml`, is transcribed from
the screenshots by eye — not exported from any run of the pipeline it judges.

Until it exists, `make gate-roster` skips, the same way `make gate` skips an
unpulled corpus and `make gate-m4` skips an untranscribed VS capture.

## Why it is transcribed rather than exported

Checking a pipeline against its own output proves nothing. This project has
already paid for that lesson once: a screen labelled `vs` stayed wrong for
three weeks because every `vs` frame matched the `vs` anchors, which had been
cropped from that same screen — a single self-consistent label has nothing to
disagree with it. `expected.yaml` is the thing that disagrees.

So: read the numbers off the pixels. Do not paste them from `control
ingest`'s summary, from a `members` or `review_queue` query, or from a
previous run of this gate.

## The transcription rule

**A value is recorded only if it reads identically in every frame it appears
in.** Capture 1 carries roughly 3.8x overlap — 331 row bands across 61
member-list frames, for around 96 members — so this is a real check, not a
formality. Where two frames disagree, or a glyph is genuinely ambiguous at
full resolution, mark it rather than guess: put the disagreement in a `note`
on the model of the VS fixture's thirteen decorated names, each recorded as
"a best reading rather than a certain one."

## What makes this ground truth, and what does not

`fixtures/m4gate/README.md` calls its `expected.yaml` "the one artifact in
this repo that cannot be generated." That claim is about *provenance*, not
about species: what makes either file ground truth is that it is read off the
pixels through a path that shares nothing with the pipeline it judges.

The eye-check that failed twice in this milestone — once on the VS name crop,
once on the roster name crop, both recorded in `CLAUDE.md` — was a check of
**a crop**: a rectangle whose contents the reader already knew, which is
exactly why `[icon fragment]GersonGamer` read as a confirmation rather than as
a defect. Transcribing a **full frame** is the opposite act: the screen as the
game rendered it, with no crop to be fooled by, at full resolution, through a
human reading the whole image rather than through tesseract-on-a-preprocessed-
band. The cross-frame agreement rule above is the additional guard, and it is
stronger than what the VS fixture had.

This is worth stating plainly rather than assuming it is obvious: the
transcriber and the pipeline can still be wrong the same way on a genuinely
ambiguous glyph, which is exactly what the `note` field and the mitigation in
the design doc's risk section are for. Nothing about reading a full frame by
eye is a proof; it is a different and considerably harder-to-fool method than
checking a crop, which is the property that matters here.

## Producing one

**1. Confirm the handset before capturing.** A dozing display returns a black
frame and a keyguard returns a *zero-byte* one; both fail hours later rather
than immediately. See `CLAUDE.md`'s operational-reality section.

```bash
adb shell dumpsys power  | grep -o 'mStayOn=[a-z]*'             # want true
adb shell dumpsys window | grep -o 'isKeyguardShowing=[a-z]*'   # want false
```

**2. Capture and ingest once.**

```bash
./bin/agent run-task --account <id> --task roster_capture
./bin/control ingest --capture <id printed above>
```

The ingest run is not what the gate checks — it is how you get the capture's
frame list. Unlike the VS gate, there is no `complete` requirement to satisfy
here: `roster_capture` opens whichever `chevron_collapsed` anchor it finds
next and stops once none remain, and a capture that ends with a group still
collapsed is not a defect in the capture — it is R1 Danger Zone's shape on
capture 1, and the fixture's `expanded` field is what records it rather than
hides it.

**3. Read the alliance frame.** One frame carries the alliance summary — tag,
name, leader, and the `Members: N/100` line. Record `alliance.member_count` as
the `N`, and `alliance.leader` as the name shown in the MEMBER LIST banner:
the leader occupies that banner and has no rank-group row, which is why the
loader checks `sum(group totals) + 1 == member_count` rather than equality.
Give that frame `group_key: "_alliance_summary"` in the frame list;
`roster_capture` deliberately asserts no group off it, and giving every other,
member-list frame an empty `group_key` records that same fact for each of
them.

**4. Read every rank group's sticky header**, expanded or not. Record its rank
badge, its name, and the `M` of its `N/M` count as `total` — the group's size,
not its online count. **Transcribe every group's header, including a
collapsed one**, and set `expanded: false` for any group `roster_capture`
never opened during this run. A collapsed group's header states its size
whether or not the group is open — R1 Danger Zone's real header reads `0/12`
on capture 1: the `0` is how many members are online, the `12` is the group's
size, and a group with zero members would not render a header to read at
all. So `total` is always positive, collapsed or not, and the loader rejects
anything else (`internal/ingest/roster_gate_test.go`'s shape fixture models
this directly: its own collapsed group carries a real, positive `total`,
scaled down but structurally identical to R1's). Recording that true total is
what condition 4 needs to describe the reconciliation of a group the route
saw and could not read.

**5. Open each member-list frame and transcribe every row**, for every group
`roster_capture` expanded. Record rank group, name, power, level, and the
last-active string exactly as shown — `"Online"` or an elapsed time like `"3h
ago"`. Apply the transcription rule above: where the same member's row appears
in more than one frame (capture 1's overlap is large enough that most do),
every occurrence must read the same value before it is recorded: a
disagreement means re-read the frames, not average or guess between them.

Do not transcribe members from a group that was never expanded. There is no
row anywhere in the capture to have read them from, and the loader refuses a
member listed under a group whose `expanded` field is `false` — see
`internal/ingest/roster_gate_test.go`'s `loadExpectedRoster`.

**6. Read the frame list out of the database**, the same query as the VS
gate's:

```sql
SELECT cf.seq, s.sha256, cf.offset_px
FROM capture_frames cf
JOIN screenshots s ON s.id = cf.screenshot_id
WHERE cf.capture_id = <id>
ORDER BY cf.seq;
```

`offset_px` is copied, never re-derived: it was measured against the frames as
captured, and ingest turns it into row positions, so a wrong value misaligns
every row after it. There is no object key to record — keys are derived from
the digest by `blob.Key`, so `sha256` is the whole reference.

## The file

```yaml
# Provenance only. The gate seeds its own capture row in lw_manager_test and
# never looks this id up; it is here so a puzzling row can be traced back.
capture: 1
period_key: "2026-W33"
game_version: "1.0.358"

alliance:
  tag: "OrCa"
  name: "Organized Chaos"
  member_count: 97
  leader: "RobElr"

frames:
  - seq: 0
    sha256: "3f786850e387550fdab836ed7e6dc881de23001b3ef7ec6ab7d0f2f5a0c0a1b2"
    offset_px: 0
    group_key: "_alliance_summary"
  - seq: 1
    sha256: "89e6c98d92887913cadf06b2adb97f26cde4849b0d4ebd5f34ff2c9f7ac6bd1a"
    offset_px: 0
    group_key: ""

groups:
  - rank: "R4"
    name: "This Is It"
    total: 9
    expanded: false
  - rank: "R3"
    name: "Footloose"
    total: 64
    expanded: true
  - rank: "R2"
    name: "I'm Alright"
    total: 11
    expanded: true
  - rank: "R1"
    name: "Danger Zone"
    total: 12
    expanded: false

members:
  - rank: "R3"
    name: "Lothar232"
    power: 225000000
    level: 34
    last_active: "10m ago"
```

**Capture 1 leaves TWO groups collapsed, not one.** R4 "This Is It" reads
`2/9` with the up chevron on seq 1 -- the only frame in the capture that shows
its header -- and R3's header follows it immediately with no rows between, so
R4 is closed exactly as R1 is. The chevron's polarity is not assumed: R3
(`10/64`) and R2 (`1/11`) carry the down chevron and are each followed by
exactly as many rows as their header states, counted; R4 and R1 carry the up
chevron and are followed by none. That is why `expected.yaml` transcribes 75
members and not the 84 an earlier reading expected, and why `expanded` is a
field rather than an assumption.

At least twenty members are required (`gateRosterMinMembers`), for the same
reason `fixtures/m4gate/README.md` requires twenty rows: below that, one bad
member already breaks the 95% threshold and the percentage stops meaning
anything. `game_version` is required for the same reason it is required
there: it is what later explains a gate that used to pass and now does not.

## What the gate then asserts

The four conditions from the design doc's §4
(`docs/superpowers/specs/2026-08-20-m4-roster-gate-design.md`):

1. at least 95% of transcribed members exist in `members`, attributed to the
   right rank group;
2. zero splits — no two `members` rows correspond to the same transcribed
   member, and no `members` row corresponds to nobody;
3. the count of transcribed members never created is no greater than the
   count of name-class `review_queue` rows — nothing dropped silently, as
   far as a count comparison can show it, since a review row records a
   screen position and its raw text, not a member, so it cannot name which
   missing member it stands for;
4. `RosterResult.PerGroup` and `Status` describe what was parsed, truthfully:
   a `partial` capture that correctly reports a short group **passes**, and a
   `complete` capture with a group missing, or a group whose `Expected`
   disagrees with the transcribed header, **fails**.

Condition 2 is this gate's counterpart to `gate-m4`'s "no misattribution," and
the difference is not stylistic: the VS route matches into a closed set of
members that already exist, while the roster route creates them. A wrong VS
read lands on the wrong existing row; a wrong roster read can mint the same
person twice, and their facts then split across two rows with no
review-queue resolution able to rejoin them. Condition 4 inverts `gate-m4`'s
"reconciles to complete" for the matching reason: demanding `complete` here
would make the gate unable to go green until the route is perfect, and a gate
that cannot go green is not a ratchet.

The denominator for condition 1 is **transcribed members, not the alliance
count**. Those differ by R1's collapsed group, R4's collapsed group and the
leader's banner row (97 − 75 = 22 = 12 + 9 + 1), and none of it is anything
the pipeline could read from these pixels — scoring against the alliance
count would be scoring against frames the capture does not contain.

## Frames are not in git

The PNGs live in the blob store, like the corpus and the VS gate's frames —
`fixtures/**/*.png` is gitignored. `expected.yaml` is a `.yaml` file and falls
outside that rule, so it commits normally.

The gate reads frames through the blob store named by `LW_BLOB_*`, so it must
run against the store the capture was written to. When a frame is missing it
skips and says which one, rather than failing as though the numbers were
wrong.
