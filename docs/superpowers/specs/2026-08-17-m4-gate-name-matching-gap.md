# M4 gate: the name-matching gap

**Status:** open. M4's collection tier works end to end; its gate does not pass.
**Measured:** 2026-08-17, against capture 6 (game version 1.0.358, period 2026-W34).

`make gate-m4` reports **57/86 rows (66.3%)** against a 95% bar. This records
what was measured on the way there, so the remaining work starts from numbers
rather than from a fresh investigation.

## What the first real gate run was worth

The gate had never executed against a capture before today — it was committed
as "the M4 gate, minus the capture it has to be pointed at". Pointing it at one
found three defects in the first run, none of which any device-free test could
have caught, and two of which had a passing "verified" note against them:

1. **The name crop started 43px inside the avatar.** Every name read carried a
   prefix of thumbnail noise — `at GersonGamer`, `ry} Leroy Jenkins 0914`,
   `T}| ArmArak` — which dragged the fuzzy score out of auto-accept.
2. **The name crop's right edge truncated the longest names.** It ended at
   0.63; `MoreBallsThanBrains` runs to 0.66.
3. **The points crop started inside the alliance line.** `Organized Chaos`'s
   tail is what became the leading dash in `— 17,219,876`, which `ParsePoints`
   correctly refused.

All three had been "verified against eight real rows by eye". Eight rows
checked by a reader who already knows what they say is not a measurement: a
crop wrong by 40px still shows a name a human reads correctly. They are now set
from an ink profile over all 142 row bands of capture 6 — see the comment on
the frac constants in `internal/ingest/vs.go` for the profile itself.

Fixing them took the gate from **0/86 to 55/86**.

## What is left, and why tuning will not fix it

Two further changes were measured and landed, and between them they were worth
two rows: homoglyph folding in `roster.Normalize` (+1) and a re-measured
`vsNameOptions` upscale factor (+1). The important output of both was not the
rows; it was the ceiling they established.

`vsNameOptions` was re-measured because options fitted to a crop that included
an avatar are not evidence about the crop that does not. The sweep ran all
eight skip-flag shapes at upscale 2/3/4 over all 142 bands, scoring each by how
many distinct members its reads auto-accepted against the 86 hand-transcribed
names:

| shape | upscale | distinct members | bands accepted | empty reads |
|---|---|---|---|---|
| gray | x2 | **62/86** | 103/142 | 15 |
| gray+inv | x2 | 62/86 | 103/142 | 15 |
| gray | x3 | 61/86 | 99/142 | 15 |
| gray+thr | x2 | 54/86 | 85/142 | 21 |
| full | x2 | 22/86 | 32/142 | 56 |

The expectation going in was that thresholding would now help, since the only
reason it had been skipped — that it destroyed a crop dominated by a colourful
avatar — was gone. It does not. It costs eight members and doubles the empty
reads even on the clean crop. Recorded because it is a plausible thing to try
again.

**The ceiling is 62/86 = 72%.** No preprocessing setting reaches the 95% bar,
because 15 of 142 bands read back *empty* at every setting.

## The two remaining fixes

The 29 failing members split cleanly, and the split is what decides the work:

### 1. Non-Latin names (~10 members)

`한씨아저씨`, `٣١٢ A l i ٣١٢`, `Danny 狂`, `Guts ツ`, `ϟϟ Leo ϟϟ`, `Aureum ⊂6М`,
`ZãP ꙅઉ`, `ZeroOrca`. These are the empty reads. An English-only tesseract
cannot return them at any preprocessing setting, and no threshold change makes
an empty string match.

The fix is tesseract language data — `-l eng+kor+ara+chi_sim` — which is
compatible with `CGO_ENABLED=0` because tesseract is a subprocess, not a
linked library. The cost is an install-time dependency that the Quickstart and
CI both have to grow, which is why it was not done today.

Note that homoglyph folding already handles the *lookalike* cases and is not
the same problem: `ΔKΔŽΔ` now folds to `akaza` and matches. A Korean name has
no Latin equivalent to fold to, and folding it away to nothing would let two
unrelated members collapse onto one key.

### 2. Single-character OCR confusions (~14 members)

`Beangraftf`/Beangraff, `AnthraxVIll`/AnthraxVIII, `JPAS`/JPA9,
`ALBANSO`/ALBAN80, `Bwize21`/Bwiz21, `ZelL1`/ZeL1, `Drizzlerst2`/Drizzlers12.
Every one is a plain ASCII name the crop renders cleanly and OCR reads with one
or two character substitutions, landing between `ReviewFloor` (75) and
`AutoAccept` (92).

The fix is confusable-aware scoring in the matcher: treating `l`/`I`/`1`,
`O`/`0`, `S`/`5`/`9`, `B`/`8`, `t`/`f` as near-equal *when scoring*, in the same
spirit as `Normalize`'s homoglyph folding but for OCR's confusions rather than
unicode's lookalikes.

**Doing this by lowering `AutoAccept` instead would be a mistake.** The
threshold is what stops a misread row being attributed to the wrong member, and
a wrong attribution writes one member's score onto another's row — the worst
failure this pipeline has, and unlike a queued row, an unrecoverable one. A
targeted distance function raises the score of reads that are genuinely the
same name; a lower threshold raises the score of everything.

## What is not in question

- The capture route works: 21 frames, `status=complete`, reconciled.
- Condition 2 passes: every discrepancy produced a review row. Nothing was
  dropped silently, which is the property the gate exists to defend.
- Condition 3 passes: the capture still reconciles to complete after ingest.

The failure is confined to condition 1 — how many rows are attributed *without
a human* on a cold run against a member list with no aliases yet. Every
unattributed row is in the review queue, where resolving it in `agent studio`
writes an alias, and the alias makes the following week's run match. So the
gate measures the hardest case the pipeline ever faces, on purpose.

Whether 95% is the right bar for a cold run — as against, say, 95% of rows
*correctly handled*, counting a queued row as handled rather than failed — is a
question for whoever picks this up. The number to beat is recorded either way.
