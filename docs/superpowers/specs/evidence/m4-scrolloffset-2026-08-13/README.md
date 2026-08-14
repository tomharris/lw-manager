# ScrollOffset on real lists — evidence, 2026-08-13

`roster_capture` failed twice on the handset with `best placement scored 0.866,
below 0.90`, then `0.846`. Two consistent readings look like a threshold set
slightly too tight. They are not: a consistent score shows consistency, not
correctness, and lowering a threshold on that evidence alone would have been
accepting an unexamined number to silence an alarm.

Measuring the full score-vs-offset curve instead found a structural defect that
the threshold was, accidentally, the only thing hiding.

## The defect

A probe strip cut from `cur` is searched **downward only** in `prev`, bounded by
the region's bottom, so a probe at `stripY` can measure at most

```
limit = regionBot - stripY - stripH
```

Probes sat at `regionTop + (p+1)*stripH`, so each successive probe reached less
far than the last, and `bestProbeStrip` chose among them **by variance** — a
criterion unrelated to whether a probe can see the answer.

A probe whose limit falls short of the true travel does not fail. The true
placement is absent from its window, so it returns the best thing present: a
**lattice decoy**. Uniformly-pitched rows correlate with themselves once per
row, so the strongest wrong answers sit at exactly ±1, ±2, ±3 row pitches.

## The measurement

Frames 01 and 02 are one swipe apart on the VS weekly ranking. The true offset
is **665px**, established independently of any matcher by reading rank 6's
position off both frames (y=1010 → y=345).

| probe | limit | reported | verdict |
|---|---|---|---|
| 0 | 866 | d=665, margin 0.156 | correct |
| 1 | 748 | d=665, margin 0.223 | correct |
| 2 | 630 | d=282, margin 0.064 | **blind — 3 row pitches off, reported with no error** |

The roster list was never healthy either, only lucky: travel runs 326–359
against probe 2's limit of 392, passing by 33px. A slightly stronger fling puts
it in the same failure.

## The negative

Frames 03 and 04 are a deliberate over-swipe (700px in 250ms, ~3,200px of
travel), so that **no correct answer exists in any probe's window**. Without a
case like this, any threshold is passable — the same reason `_none` frames are
part of the corpus gate.

Each probe returns a different answer, each pinned near its own limit and
therefore exactly one row pitch from its neighbour: **757 / 629 / 502**.

## What this proves about the acceptance criteria

| | probe-0 peak | probe-0 margin | probes agree | candidate vs limit(0) |
|---|---|---|---|---|
| roster, 5 real pairs | 0.827–0.908 | 0.043–0.117 | yes, within 1px | 340 vs 634 |
| VS, real pair | 0.849 | 0.156 | yes (probe 2 excluded) | 665 vs 866 |
| over-swipe, garbage | 0.706 | 0.049 | **no — 757/629/502** | **757 vs 866** |

Two things follow, and both are the opposite of the obvious guess:

- **Margin does not discriminate.** The worst real margin (0.043) is lower than
  the garbage case's (0.049). A margin threshold cannot be the defence.
- **No absolute floor can separate a right answer from a wrong one within a
  pair**, because a lattice decoy reaches 0.86 while a true peak reads 0.83.

What does separate them is geometry: a probe that cannot reach the candidate is
excluded by arithmetic rather than by score, and probes pinned near their own
limits disagree. `0.90` was never the bug; it was a tripwire over a hole.

## Rejected: averaging the probes

Averaging the three curves improves the roster (worst margin 0.035 → 0.046, max
decoy 0.861 → 0.817) and **destroys** the VS case, returning d=538 at a margin
of 0.0041 — an answer neither honest probe produced — because it lets a blind
probe outvote a sighted one. Recorded here so the experiment is not repeated.

## Reproducing

`internal/vision/scrolldiag_manual_test.go`, build tag `scrolldiag`:

```bash
go test -tags scrolldiag ./internal/vision -run TestScrollDiag -v -args \
  -frames 01-vs-weekly-frame-0.png,02-vs-weekly-frame-1-after-swipe.png \
  -pitch 128 -y1 0.185 -y2 0.80 -true 665
```

Files 05–07 are the raw output. 06 and 07 predate the corrected probe placement
and are kept because they are what the failure looked like from inside.
