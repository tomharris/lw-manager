# M4 gate: the name-matching gap

**Superseded** by `2026-08-17-m4-closed-set-matching-design.md` for everything
under "What is left". Its account of how the name field reached 71/86 stands,
and so does its lesson about two aggregates read as one causal claim.

**Status:** open. M4's collection tier works end to end; its gate does not pass.
**Measured:** 2026-08-17, against capture 6 (game version 1.0.358, period 2026-W34).

`make gate-m4` reports **63/86 rows (73.3%)** against a 95% bar, up from 57/86
(66.3%). This records what was measured on the way there, so the remaining work
starts from numbers rather than from a fresh investigation.

Conditions 2 and 3 pass throughout: 23 discrepancies produced 23 review rows,
nothing was dropped silently, and the capture still reconciles to complete.

## The first revision of this document was wrong, and the way it was wrong is the lesson

It recorded that the 15 row bands reading back empty were the ~10 non-Latin
names, and concluded that tesseract language packs were the fix worth ~10
members. Both halves were wrong, and they were wrong in a way that looked
thoroughly evidenced: two true aggregates — "15 bands read empty" and "this
roster has about ten names in non-Latin scripts" — were laid next to each other
and read as one causal claim. Nothing checked whether they were the same set.

They were not. Per member, the empty bands belonged almost entirely to **plain
ASCII names**: `Drizzlers12`, `2Rule`, `Mc1999`, `IamIronman2025`, `Beinsee`,
`Syłar`, `Mar 89`, `Bujangann`, `Nichoj`, `Delio1`, `AA91AA`, `ZeroOrca`. The
genuinely non-Latin names were never empty at all — `Danny 狂` read as
"Danny 3t", `Guts ツ` as "Guts ‘V", `한씨아저씨` as something scoring 66. A
language pack would not have moved a single one of the empty bands.

This is the same failure CLAUDE.md already describes for `worst-in` being a
minimum over frames, arrived at from a different direction: **an aggregate can
be perfectly accurate and still support the wrong story, and only a per-item
view can tell you which story it supports.**

The instrument that settles it is now committed (see below), which the previous
one was not.

## The probe is committed this time

`internal/ingest/zz_name_probe_test.go`, run with `make probe-m4`. It reads
every row band of the gate's capture through the production read path and
scores it against the 86 hand-transcribed names, reporting distinct members
auto-accepted, bands accepted, empty reads, and — with `-probe.detail` — a
per-member line naming the best read and why it lost.

The numbers that set `vsNameOptions` originally came from a harness that was
never committed, and rebuilding it cost most of a session. It also earns its
place beyond that: two of the three fixes below were found by looking at its
per-member output, and one of its own sweeps was caught measuring nothing (see
"the harness lied" below).

## What was fixed, and what each was worth

Measured through `make probe-m4`, all on the same 142 bands and 86 names:

| change | distinct members | bands | empty |
|---|---|---|---|
| baseline (reproduces the previous session exactly) | 62/86 | 103/142 | 15 |
| + raw-line retry on empty reads | 64/86 | 106/142 | 0 |
| + confusable-aware scoring, and a rune-length fix | 69/86 | 114/142 | 0 |
| + retry preprocessing fitted rather than inherited | **71/86** | 117/142 | 0 |

### 1. Tesseract's layout analysis is blind to some legible crops

The root cause of every empty read. The crops are bold black text on a light
grey field, correctly framed, with generous margins — and PSM 3, 4, 6, 7, 11
and 12 all return the empty string. PSM 8 ("single word") and PSM 13 ("raw
line, bypassing hacks that are Tesseract-specific") both read them. The fault
is in layout analysis, before recognition runs at all.

PSM 13 is the usable one: PSM 8 would collapse the letter-spaced names
(`M I C H E L L`) that `roster.Normalize` exists to handle. But switching to it
outright is far worse — 31/86 members against PSM 7's 62/86 — so it is confined
to crops that produced *nothing at all*, where there is no read to lose.

`internal/ocr/testdata/psm7_layout_blind.png` is the case reduced to one file:
2KB, reads empty at PSM 7, reads "Bujangann" at PSM 13.

The retry lives in `ingest.readFieldWithRetry`, not in the engine, because
whether a poor read beats no read is a property of the **field**. A name has a
known roster behind it, so a bad read simply fails to match. A number does not:
an empty points read fails safely to the review queue, while a raw-line retry
could manufacture a plausible value out of a crop that caught neighbouring
content — the failure `vsPointsSpec`'s charset comment already documents.

### 2. Confusable-aware scoring

`internal/roster/confusable.go`. Edit distances now run in tenths of an edit,
and a substitution between a pair OCR interchanges costs 2 rather than 10.
Only substitution is cheapened: a confusable pair is evidence the engine saw
the right glyph and named it wrongly, while an insertion or deletion is
evidence it saw something absent or missed something present. Those are
different claims and do not get the same discount.

`confusableCost = 2` is fitted, not chosen. The binding cases are the short
names, where one edit is proportionally huge: `AA91AA` is six characters with
two confusable substitutions and needs 100*(60-2c)/60 >= 92, so c <= 2.4.

**The safety half is measured too, and matters more.** `roster.ClosestPairScore`
reports the highest score between any two *distinct* real names on the roster;
`make probe-m4` prints it and fails if it reaches `AutoAccept`. On this roster
it is **60**, a 32-point margin. Cheapening substitutions buys matches by
spending separation between real members, and that budget has to be shown, not
assumed — a wrong attribution writes one member's score onto another's row, the
one failure here a human cannot undo later.

Lowering `AutoAccept` instead remains the wrong move, for the reason the first
revision of this document already gave.

### 3. A latent scoring bug: rune distance over byte length

`ratio()` computed Levenshtein over `[]rune` but divided by `len()` — the byte
length. For ASCII the two agree, so it never showed. For a multi-byte name the
denominator was up to three times too large, which **inflates** the score:
`한씨아저씨` scored 66 against the unrelated read "AKAZA" for no reason but this.

This was making false near-misses most likely on exactly the non-Latin names
that are hardest to read, and any cost constant fitted against those numbers
would have been fitted to an artifact. Fixed before fitting anything.

### 4. The retry's preprocessing was inherited, not measured

The retry first reused the primary's options, on the unexamined assumption that
only the segmentation mode needed to change. Sweeping the retry's own options
across all eight shapes at upscale 2/3/4, with the primary held fixed:

	gray+inv  x4   71/86 distinct   (117/142 bands)  <- chosen
	gray      x4   70/86            (116/142)
	gray      x2   69/86            (114/142)        <- what inheriting gives
	full      x2   66/86            (110/142)

Upscale 4 is most of it; inverting is worth one more. Two members on fifteen
bands is a real result on this capture rather than a large one — re-run
`make probe-m4 PROBE_ARGS=-probe.fbsweep` on a later capture and believe the
newer measurement.

### The harness lied once, and the tell was agreement

The first run of that sweep returned **24 identical rows**. It read as a clean
negative result: "preprocessing does not matter for the retry." It was
measuring nothing — the retry was happening inside the engine, with the
primary's options, before the probe ever saw an empty string.

What gave it away was not disagreement but *implausible agreement*: `full x2`
and `gray x4` differ enormously for the primary read, so identical retry
results were not a physically plausible outcome. **A broken instrument reports
uniformity, which is easy to mistake for a finding.** Suspicious agreement
deserves the same scrutiny as suspicious disagreement.

## What is left: two buckets, and they are not the same problem

### Names (16 rows)

Ten reach `no_confident_match`, six land in the 75–92 review band. Within them:

- **Genuinely non-Latin (4-6 members).** `한씨아저씨` (Korean), `٣١٢ A l i ٣١٢`
  (Arabic), `Danny 狂` (CJK), `Guts ツ` (katakana), and possibly `Aureum ⊂6М`
  and `ZãP ꙅઉ`. This is the language-pack fix, and it is now the *only* thing
  language packs are claimed to be worth — a much smaller number than the ~10
  the first revision estimated.

  The plumbing is in place and tested: `ocr.Spec.Languages` emits `-l`, and
  `TesseractEngine.InstalledLanguages`/`MissingLanguages` report which packs
  are present, because a missing pack is not an error to tesseract — it falls
  back silently and returns a worse read. Nothing sets `Languages` yet, because
  the packs are not installed on this machine (`tesseract --list-langs` returns
  `eng` and `osd` only). To measure:

      sudo apt install tesseract-ocr-kor tesseract-ocr-ara \
                       tesseract-ocr-chi-sim tesseract-ocr-jpn
      make probe-m4 PROBE_ARGS='-probe.langs=eng+kor+ara+chi_sim+jpn -probe.detail'

  Measure before wiring it: multi-language tesseract is known to trade English
  accuracy for coverage, and 117 of 142 bands currently read correctly in
  English. The probe reports both halves.

- **Hard reads (~10 members).** `Drizzlers12` → "brieelerst2@", `Mc1999` →
  "mas99", `Delio1` → "beliot", `2Rule` → "erule", `Syłar` → "cular". These are
  the raw-line retry's own output, which is noisier than a primary read by
  construction. No language pack or threshold touches them.

- **One genuine anomaly.** `IamIronman2025` is `LOST` at 35: with zero empty
  bands, its row reads *something*, but nothing resembling the name. Worth one
  look at its band before assuming it is an OCR problem — it may be a
  transcription or segmentation question instead.

### Points (7 rows) — newly characterized, previously counted as "unexplained"

These rows' **names matched fine**; no fact was written because the points
field failed. This bucket was invisible until the name side improved enough to
expose it.

	low_confidence_points | 8,835,180     Handbol      want 8835180   exactly right, rejected
	low_confidence_points | 1,242,375     Nichoj       want 1242375   exactly right, rejected
	low_confidence_points | 18,356,304    Mar 89       want 18356804  genuinely wrong (0 for 8)
	unparseable_points    | ¢,609,299     albambet     want 2609299   leading 2 misread
	unparseable_points    | e,2¢8,001     ZeL1         want 2328001   2 misread twice
	unparseable_points    | (empty)                                   layout-blind, as above
	unparseable_points    | (empty)                                   layout-blind, as above

Read the first three together before treating this as free accuracy. Accepting
all three low-confidence reads would gain two correct values and write **one
wrong number** to a leaderboard — so the confidence gate is working, and this
bucket is a real precision/recall trade on the one metric the pipeline cannot
take back, not an oversight.

The two empty reads are the same layout blindness fixed for names, and the
retry was deliberately withheld here. Extending it to a numeric field needs its
own argument and its own measurement, not an appeal to the name-side result.

## The bar

Whether 95% is right for a **cold** run — against a roster with no aliases,
where every unmatched row goes to a queue whose resolution writes an alias that
makes next week's run match — remains open, and is now a more pressing
question: names and points together account for 23 rows, and the reachable
fixes named above are worth perhaps 10 of them. The gate measures the hardest
case the pipeline ever faces, on purpose.

`fixtures/m4gate/expected.yaml` is the hand-transcribed ground truth and is the
one artifact that cannot be regenerated — 86 rows read by eye off 21 frames. Do
not "refresh" it from ingest output.
