# The M4 gate's hand-checked capture

`make gate-m4` runs ingest over one real VS weekly-ranking capture and checks
the numbers it produces against `expected.yaml` in this directory. That file is
the ground truth and is **the one artifact in this repo that cannot be
generated** — it is transcribed from the screenshots by eye.

Until it exists, `make gate-m4` skips, the same way `make gate` skips an
unpulled corpus.

## Why it is transcribed rather than exported

Checking a pipeline against its own output proves nothing. This project has
already paid for that lesson once: a screen labelled `vs` stayed wrong for
three weeks because every `vs` frame matched the `vs` anchors, which had been
cropped from that same screen — a single self-consistent label has nothing to
disagree with it. `expected.yaml` is the thing that disagrees.

So: read the numbers off the pixels. Do not paste them from
`control ingest`'s summary, from a `participation_facts` query, or from a
previous run of this gate.

## Producing one

**1. Confirm the handset before capturing.** A dozing display returns a black
frame and a keyguard returns a *zero-byte* one; both fail hours later rather
than immediately. See CLAUDE.md's operational-reality section.

```bash
adb shell dumpsys power  | grep -o 'mStayOn=[a-z]*'             # want true
adb shell dumpsys window | grep -o 'isKeyguardShowing=[a-z]*'   # want false
```

**2. Capture and ingest once.**

```bash
./bin/agent run-task --account <id> --task vs_capture
./bin/control ingest --capture <id printed above>
```

The ingest run is not what the gate checks — it is how you get the capture's
frame list and confirm the capture reached the bottom of the list
(`status=complete` in the summary). A `partial` capture is not usable as
ground truth: absence and truncation are indistinguishable on one, so the gate
cannot tell a member who scored nothing from a member the scroll never reached.

**3. Read the frame list out of the database.**

```sql
SELECT cf.seq, s.sha256, cf.offset_px
FROM capture_frames cf
JOIN screenshots s ON s.id = cf.screenshot_id
WHERE cf.capture_id = <id>
ORDER BY cf.seq;
```

`offset_px` is copied, never re-derived: it was measured against the frames as
captured, and ingest turns it into row positions, so a wrong value misaligns
every row after it.

There is no object key to record. Keys are derived from the digest by
`blob.Key`, so the `sha256` is the whole reference.

**4. Open each frame and transcribe every row.** `agent studio` will open a
frame by hash. Record rank, name and points exactly as they read on screen.

Rank is not asserted by the gate — facts are keyed by member, not by screen
position — but transcribe it anyway: it is what makes a skipped or doubled row
obvious when the list is checked over.

## The file

```yaml
# Provenance only. The gate seeds its own capture row in lw_manager_test and
# never looks this id up; it is here so a puzzling row can be traced back.
capture: 42
period_key: "2026-W33"
game_version: "1.0.357"

alliance:
  tag: "ABC"
  name: "Example Alliance"

frames:
  - seq: 0
    sha256: "3f786850e387550fdab836ed7e6dc881de23001b3ef7ec6ab7d0f2f5a0c0a1b2"
    offset_px: 0
  - seq: 1
    sha256: "89e6c98d92887913cadf06b2adb97f26cde4849b0d4ebd5f34ff2c9f7ac6bd1a"
    offset_px: 612

rows:
  - rank: 1
    name: "Lothar232"
    points: 73614570
  - rank: 2
    name: "BobLeeSwagger44"
    points: 65336176
```

At least twenty rows are required. Below that, one bad row already breaks the
95% threshold and the percentage stops meaning anything — the same reason the
M1 gate insists on at least 200 corpus frames.

`game_version` is required for the reason `fixtures/corpus/index.yaml` carries
it: it is what later explains a gate that used to pass and now does not. A
39.7 MB game update landed mid-recon, so this is live rather than theoretical.

## What the gate then asserts

1. parsed points within 1% of the hand-checked value on ≥95% of rows;
2. every discrepancy produced a `review_queue` row — none dropped silently;
3. the capture still reconciles to `complete` after ingest.

Condition 2 is the one that makes this a gate rather than an accuracy score. A
pipeline that quietly drops the rows it finds hard scores well on condition 1
alone, and quiet dropping is the failure this milestone exists to prevent.

## Frames are not in git

The PNGs live in the blob store, like the corpus — `fixtures/**/*.png` is
gitignored. `expected.yaml` is a `.yaml` file and falls outside that rule, so
it commits normally.

The gate reads frames through the blob store named by `LW_BLOB_*`, so it must
run against the store the capture was written to. When a frame is missing it
skips and says which one, rather than failing as though the numbers were wrong.
