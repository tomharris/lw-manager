# lw-manager — conventions and invariants

Last War automation and alliance analytics platform. The architectural thesis:
**the bot is the collection tier for the analytics tier.** A bot that can
navigate to the alliance roster and to the VS ranking can screenshot those
screens on a schedule and feed the parser directly.

Those are two separate routes, not one chain. The roster is Alliance →
Members. The VS ranking is **not reachable from Alliance at all**: it is
base → VS → Alliance Duel → Ranking → "weekly ranking" tab → select your
alliance. VS is the button, not the screen — it lands on a screen titled
ALLIANCE DUEL, and eliding that step is exactly the conflation that let a
screen labelled `vs` stand uncorrected for three weeks (see the corpus
section). Earlier docs describe the whole route as "Alliance → Members → VS
Ranking", which is wrong and is worth correcting wherever it appears — a
route that does not exist is the kind of thing a task gets written against.

Full design: `docs/lastwar-platform-design.gen`.
Current milestone spec: `docs/superpowers/specs/`.

**The milestone order is M0 → M1 → M2 → M4 → M5, then M3 if ever.** M3 (Fleet)
is deferred behind M5 as an enhancement that may not get built. It is still
called M3 — the numbers are cited by name in this file, in the corpus notes,
and in spec filenames, so renumbering would break more than it fixes. Most of
what M3 listed already exists (`agent register`/`agent accounts` is the
registry, `internal/scheduler` holds per-account cadences, `agent run` already
drives every attached serial); the deferred part is the dashboard and the
WebSocket status feed, which nothing in M4 or M5 depends on.

## Quickstart

```bash
docker compose up -d          # postgres on :5433, minio on :9000
make build                    # bin/agent, bin/control
./bin/control migrate

# with an emulator running and `adb devices` listing it:
./bin/agent devices                                  # confirm the probe works
./bin/agent register --nickname myalt --role alliance_data
./bin/agent capture --account <id printed above>
./bin/agent run-task --account <id> --task help_all   # one task now, no scheduler
./bin/agent run                                       # scheduler loop, all attached devices
./bin/control pause --reason "alliance event"         # global kill switch
./bin/control resume
./bin/agent accounts                                 # what is registered

./bin/control alliance set --tag <tag> --name <name>  # the one tracked alliance, once
./bin/control ingest --capture <id>                   # capture -> facts (M4); --period overrides

./bin/agent record --interval 2s --duration 10m   # burst-capture the corpus
./bin/agent studio --addr 0.0.0.0:8088            # label and crop, from a browser
./bin/agent corpus index && ./bin/agent corpus push
./bin/agent score                                 # the M1 gate, with diagnostics
./bin/agent score --json                          # + per-frame predictions, to localize a failure
make gate                                         # the same gate, as a test

make gate-m4                                      # the M4 gate: ingest vs a hand-checked capture
make probe-m4                                     # measure the name field; not a gate, read the output
make probe-m4 PROBE_ARGS='-probe.detail'          # per-member, to localize
make probe-points                                 # measure the points field; also not a gate
make probe-assign                                 # measure closed-set matching; also not a gate
make probe-roster                                 # measure the ROSTER name field; also not a gate
```

`register` probes the device over adb rather than taking a resolution flag:
registration is the one cheap moment to prove the serial is real and the
device is reachable. It is idempotent, so re-running it corrects a typo
instead of creating a duplicate account.

## Invariants — non-negotiable

1. **No absolute pixel coordinates outside a `Transport` implementation.**
   Everything upstream speaks `transport.Norm` (both components in `[0,1]`).
   `Norm.Pixels` is the only sanctioned denormalization point.
2. **Every task is idempotent and interruptible.** Assume the process is
   killed at any step. Re-running a half-finished task must be safe.
3. **No task acts without a matched screen anchor first.** Blind taps are a
   bug, not a shortcut.
4. **Facts are append-only.** Corrections supersede via `superseded_by`;
   nothing is mutated in place. Every number must trace back to a screenshot.
5. **Every OCR-derived number carries a confidence and a screenshot reference.**
   Low-confidence reads go to the review queue, never to a leaderboard.
6. **All vision logic ships with fixture-based tests that run with no device
   attached.** `go test ./...` must pass with no emulator, no adb, no Docker.
7. **Sleeps go through the jittered context helper.** Never bare `time.Sleep`
   in task code — fixed timing is the most detectable signal we emit.
8. **The kill switch is checked between every task step.** Global `PAUSE_ALL`
   and per-account `enabled`. You will need to stop everything in five seconds
   during an alliance event.

## Go conventions

- **`CGO_ENABLED=0`, always.** Enforced by the Makefile and `make verify-nocgo`.
  This rules out gocv, gosseract, and onnxruntime_go. It is a deliberate
  trade: OCR goes through the `tesseract` CLI as a subprocess, and template
  matching is a hand-rolled NCC implementation.
- `context.Context` is the first parameter of anything that does I/O.
- Wrap errors with `%w` and enough context to locate the failure without a
  stack trace: which device, which account, which key.
- All output goes through `log/slog` to **stderr**. CLI results go to stdout
  so they stay pipeable.
- Sentinel errors (`ErrNotFound`, `ErrOutOfRange`, `ErrAccountDisabled`) are
  compared with `errors.Is`/`errors.As`, never by string.

## Testing

- `make test` — unit tests. **Must pass with nothing running.** Fakes:
  `transport.ReplayTransport`, `blob.FSStore`, and package-local store fakes.
- `make test-integration` — needs `docker compose up -d`. Tagged
  `//go:build integration`. Runs against **`lw_manager_test`**, never the dev
  database — see below.
- `make test-device` — needs an emulator or handset on adb. Tagged
  `//go:build device`, kept separate from `integration` because the
  infrastructure differs: adb, not Docker. Skips when no device is attached.
  This is the only place `ADBTransport` is exercised for real.
- `make probe-m4` — **not a gate**: a measuring instrument for the VS name
  field, which asserts nothing and always passes. It reads every row band of
  the gate's capture through the production read path and scores it against
  the 86 hand-transcribed names. Reach for it before changing any crop,
  preprocessing option, page-segmentation mode or matcher constant — and after,
  because "re-measure, do not re-reason" applies to all of them. Tagged
  `m4probe`, needs the blob store and tesseract, no database.
  `-probe.detail` is the per-member view, `-probe.sweep` and `-probe.fbsweep`
  the option grids for the primary read and the retry.
- `make probe-points` — **not a gate**: the same instrument for the points
  field. It reads every band and reports rows exact, bands exact, within 1%,
  unparseable, empty, low-confidence and retried, so a change can be read
  against the column it was supposed to move. Note what it cannot tell you:
  it attributes a band to a rank **by value**, so a misread band matches no
  rank and simply drops out of the exact count — it can never show you what
  a wrong number actually said. `-points.detail`, `-points.sweep`,
  `-points.psm` and `-points.fbsweep` mirror the name probe's flags.
- `make probe-assign` — **not a gate**: the measuring instrument for closed-set
  matching. Reach for it before changing `roster.ResidualFloor`,
  `ResidualMargin` or `residualMatchConfidence`, and after. Two of its modes
  keep its own numbers honest and both should be run before believing a
  headline: `-probe.assignshuffle` rotates the truth labels so every
  assignment is wrong by construction (it must report ~0 correct), and
  `-probe.assigndecoys=N` pads the member set, because the gate's capture is
  square and production is not.
- `make probe-roster` — **not a gate**: the roster route's counterpart to the
  three VS instruments above, and for a milestone the only route that had
  none — which is how its name crop stayed wrong while the VS crops were being
  fitted to histograms. `-roster.inkprofile` prints the column ink histogram a
  crop edge is placed from, `-roster.x0sweep` sweeps `nameXFrac0`,
  `-roster.detail` is the per-band view, `-roster.members` the per-member one,
  `-roster.noretry` measures what the shipped PSM-13 retry is worth, and
  `-roster.lastactive` / `-roster.lasweep` measure the status column split by
  which of its two states a row is in.
  It scores against `fixtures/m4rostergate/expected.yaml` — 75 members, that
  capture, transcribed frame by frame — so `exact` is an accuracy and
  `unmatched` an error rate. It did **not** used to be: until the roster gate's
  fixture existed it scored against the VS ranking's 86 ranked names, three
  days later and missing 11 of this roster's members, and every number carried
  a "lower bound, never an accuracy" caveat. Two columns still need their own
  reading: `junk-prefixed` (a known name plus one leading token — the direct
  measure of a left-edge crop defect) and `exact (below MinConf)` (read
  correctly, refused for confidence, and invisible to an accuracy count).
  **`-roster.members` is the mode that answers the gate's question**, because
  the gate reports members never created and every other mode reports bands: a
  band-keyed count and a member-keyed loss are two aggregates, and pairing them
  by hand is the inference this milestone got wrong twice.
- `make gate` — the M1 phase gate: recognizer accuracy against the real
  corpus. Tagged `//go:build corpus`, device-free but slow, so it stays out
  of `make test`. Skips when the corpus has not been pulled. Carries an
  explicit `-timeout` because it costs frames × anchors and both keep
  growing; a panic with a goroutine dump at exactly 600s is Go's default
  timeout, not a gate failure.
- New packages get a fake or a replay path before they get a real
  implementation. `ReplayTransport` was written before `ADBTransport` was
  trusted, and that ordering is the pattern to follow.

### The test database is separate, and deliberately hard to misdirect

Integration tests truncate and delete freely, so they run against
`lw_manager_test` via `internal/dbtest`, which creates and migrates it on
demand. Nothing to set up by hand.

Two properties are load-bearing:

- **Tests do not read `LW_DATABASE_URL`.** That is the application's variable;
  honouring it means a developer with it exported points the suite at real
  data. Tests read `LW_TEST_DATABASE_URL` and fall back to a default, never to
  the app's setting.
- **`dbtest` refuses any database not named `*_test`** (`ErrUnsafeDatabase`),
  checked before it connects. The guard, not the default, is what makes this
  safe.

`dbtest.Prepare` takes the migrate function as an argument rather than
importing `internal/db`: the db package's own integration tests are in
`package db`, so importing dbtest from there would be a cycle.

To start over: `docker compose exec postgres psql -U lw -d postgres -c 'DROP
DATABASE lw_manager_test'`. The next run recreates it.

### Parallel test binaries race on a clean database

`go test ./...` runs package binaries concurrently, so on a server with no
`lw_manager_test` yet, two of them reach `CREATE DATABASE` at once. The loser
does **not** reliably get `42P04` (`duplicate_database`) — Postgres reports
that only when its own name lookup catches the conflict first. A real race
surfaces as `23505` from the unique index on `pg_database`. `dbtest` re-checks
existence after a failed create rather than matching SQLSTATEs, and holds a
Postgres advisory lock across migration for the same reason.

This only ever fails on a clean database, which means it fails on CI and on a
new developer's first run and nowhere else. Test it with a `DROP DATABASE`
first, not by re-running a suite that already passed.

### ReplayTransport exhaustion

Holds its last frame once fixtures run out, but caps total serves
(`DefaultMaxServes`). Holding lets poll-until-recognized loops settle like a
real idle device; the cap makes a non-converging task fail fast rather than
hang the suite. Override per-test with `rt.MaxServes`.

### The fixture corpus lives in the blob store, not in git

600+ full-resolution screenshots is 437 MB today, which git would keep in
history forever. So `fixtures/corpus/<label>/<sha256>.png` is gitignored and
the bytes live in the content-addressed blob store; only
`fixtures/corpus/index.yaml` is committed. `agent corpus pull` materializes
them, `push` uploads new ones, `index` regenerates the projection.

**The label is the directory.** There is no sidecar metadata to fall out of
sync, so a mislabel is fixed with `mv` and the corpus is inspectable without
any of our code. `index.yaml` carries only what a PNG cannot: capture time,
device model, and **game version** — which is what later explains a gate that
used to pass and now does not.

Two properties are load-bearing:

- **A duplicate frame is dropped**, which is the opposite of the
  `screenshots`-table rule above. There, identical bytes still earn a row
  because each capture is a distinct observation. Here a duplicate is noise
  that would weight the accuracy denominator toward whichever screen the
  phone happened to idle on.
- **Negatives are part of the gate.** `_none` frames are correct only when
  recognition *fails*. Without them the gate is passable with thresholds so
  loose that every frame matches something, and acting on a misidentified
  screen is exactly the blind tap invariant #3 forbids.

**`agent record` rewrites `index.yaml` from what is on disk — so pull first.**
`corpus.Reindex` builds the index solely from a scan of the working tree; the
previous index is consulted only to carry metadata forward for hashes still
present. That is coherent with "the label is the directory", but it means
recording into a corpus that was never pulled **silently drops every labelled
entry from the committed index**, because those directories do not exist
locally. It looks like a clean 204-frame index rather than like a truncation.

Nothing is destroyed — the bytes are in the blob store and `index.yaml` is in
git — but the recovery is a *merge*, not a checkout: the new frames'
`captured_at`, `device`, and `game_version` live only in the freshly written
file, and those stamps are the whole reason the index exists. Restore the
committed index, `corpus pull`, append the new entries onto it, then
`corpus index`.

The habit that avoids all of it: **`agent corpus pull` before `agent record`,
every time, on any machine that has not pulled.**

`agent score` prints a confusion matrix and a separation report keyed on
**(anchor ID, screen)**, not on anchor ID alone: `registry.go` allows the same
anchor ID to be declared on two different screens (a `back_button` on both,
say), and collapsing those rows would let a healthy anchor mask a
non-discriminative namesake. Each row is the actionable one: a positive gap
between that anchor-on-that-screen's worst in-screen score and its best
out-of-screen score means retune the threshold to the suggested midpoint, and
a non-positive gap — or zero in-screen observations at all — means **recrop**,
because no threshold can separate them. That distinction matters because
recognition aggregates anchors by min per screen, so one bad anchor caps its
entire screen.

### Both reports are aggregates. `--json` is how you localize.

Neither the matrix nor the separation report can name a frame, and past a
certain point that is the only question left. `agent score --json` adds a
`predictions` array — one `{Hash, Label, Predicted}` per frame — and the hash
is what `agent studio` opens. Filter it with
`jq '.predictions[] | select(.Label != .Predicted)'`, remembering that a
`_none` frame predicts the empty string and is *correct*.

Reach for it when a number will not move. `worst-in` is a **minimum over
frames**, so one catastrophic frame drags it down exactly as far as forty
would: a systematically bad crop and five frames that simply do not contain
the button produce the same reading, and they need opposite fixes. Three
separate recrops chasing `base`'s `worst-in 0.145` moved nothing, because the
anchors were fine — five frames had caught the bottom nav bar mid-animation
and had no buttons in them at all. Per-frame output localized it in one pass:
all five sat in a single four-minute capture burst, 5 of 18 there against 0
of 46 elsewhere, before anyone looked at a pixel.

The corollary is that **a low `worst-in` is a hypothesis about the anchor or
about the frames, and the report cannot tell you which.** Check the frames
first — they are cheaper to inspect than a crop is to redo, and a mislabel
found is worth more than a threshold tuned around it.

### A RECROP verdict on a screen with no false positives is declinable

The separation report judges one anchor at a time and is blind to the
min-aggregation it warns you about. An anchor reading `best-out 1.000 →
RECROP` looks unsalvageable, and on a single-anchor screen it is. On a screen
with three identifying anchors and zero false-positive cells in the matrix,
the exclusion work is already being done by the other two; that anchor
contributes only a failure mode, and relaxing it cannot cost what it does not
provide. `vs_ranking_weekly_button` sat at `best-out 1.000` while
`vs_ranking_alliance` committed no false positives at all.

The matrix is ground truth for whether a screen over-claims. Read the
separation row for *why*, then decide — and prefer a recrop anyway when the
score was depressed by a transient (a focus glow, a slide-in), since a
threshold fitted to one observed animation frame is fitted to a sample of one.

### Anchors detect presence, never absence — and flat crops are near-degenerate

NCC divides out the template's variance, so a nearly-flat template correlates
~1.0 with *any* similarly flat region: it ends up asking "is this area smooth
and dark", which describes a great deal of this UI. `matcher.go` rejects
zero-variance templates outright (the `tVar` check), but there is no cliff —
a crop just above that line is nearly as useless and reports no error.

Measure before trusting a small crop. Standard deviation across three real
templates, all the same physical size band:

| template | content | stddev |
|---|---|---|
| `vs_ranking_weekly/vs_ranking_alliance_button` | empty checkbox | 2,346 |
| `vs_ranking_alliance/vs_ranking_weekly_button` | checkbox + green check | 10,324 |
| `alliance/alliance` | wordmark | 25,927 |

The empty checkbox is ~11x flatter than a text anchor and reports
`worst-in 1.000 / best-out 1.000` — it matches everywhere. `vs_ranking_weekly`
still scores 10/10, but **its header and `weekly_tab` are carrying it, not the
anchor the manifest credits**, and both of those are shared with
`vs_ranking_alliance`.

That is the general trap: a checkbox-is-unchecked anchor asks a correlation
score to confirm that nothing is there, which it has no way to express. Where
two screens differ only by the presence of something, anchor the screen that
*has* it and find another discriminator for the one that does not — do not
crop the empty state and call it an identifying anchor. A screen whose pass
comes entirely from shared anchors will accept its sibling just as readily.

The recognizer needs an identifying anchor for **every** labeled screen, not
just the six `DefaultGraph()` navigates. `alliance_members` and `vs_ranking`
are in the corpus for M4; without anchors they would be wrong on every
scoring run forever. Recognition and navigation are separate concerns.

### The screen vocabulary changed twice, for two different reasons

Both collapses are in git history, but the reasoning is worth keeping
somewhere durable, because the two failures don't look alike.

`vs_ranking_alliance` merged into `vs_ranking_weekly` for the reason directly
above: the two were the same screen, differing only by whether the "Your
Alliance" checkbox was ticked, and NCC has no way to score the *absence* of
something. Filter state moved out of screen identity entirely and into a
`Ctx.Sees` anchor query.

`vs` merged into `alliance_duel` for an unrelated reason. `vs` was a
provisional label taken from the *button* that opens the screen — base's VS
button — rather than from anything the screen itself says, and it had been
wrong since 2026-07-27. It went undetected for three weeks because **a single
mislabelled screen is perfectly self-consistent**: every `vs` frame matched
the `vs` anchors, because the anchors were cropped from that same screen, so
nothing in the corpus ever contradicted it. It only became visible once a
second label — `alliance_duel`, taken from the "ALLIANCE DUEL" header the
screen actually carries — was added and started competing for the same
pixels. A corpus with one label per screen has no way to catch a mislabel,
because a wrong label with nothing to disagree with looks exactly like a
right one.

### Action anchors were never in the gate

The M1 gate and the separation report above cover **identifying anchors
only** — `recognizer.go` skips every anchor whose `IdentifiesScreen` is
false, so no observation was ever produced for a tap target: of 59 anchors in
the manifest, 20 are measured against the gate and 39 are invisible to it. The gate proved the bot knew
where it was standing and nothing proved it could hit what it aimed at.

`agent score --actions [--screen X]` now measures them, reusing
`Separations` so the same worst-in/best-out/gap reading applies. Its known
limitation: it reports an anchor's **worst** in-screen score and never its
best, so for a *state* anchor — one legitimately absent from some frames,
like `chevron_collapsed` on a fully-expanded roster — a single minimum
cannot express the bimodality that would distinguish "correctly absent" from
"too weak to match when it is present." A RECROP verdict on an anchor like
that is not conclusive either way; it takes the per-frame view, not the
aggregate, to settle.

### A crop "verified by eye" against a handful of rows is not measured

The M4 ingest crops were recon-estimated from one frame, then checked against
eight real rows with each row read back by eye before OCR ran, and recorded as
holding. All three of them were wrong. The name crop started 43px inside the
avatar, its right edge truncated the longest names in the alliance, and the
points crop started inside the alliance line. The first run against a real
capture scored **0 of 86 rows**.

The reason the review passed is worth keeping, because it is not carelessness:
**a human reading a crop already knows what the name says.** A rectangle
containing `[avatar fragment]GersonGamer` reads as "GersonGamer, fine" to a
person and as `at GersonGamer` to OCR, and eight of those in a row look like
eight confirmations. The check confirmed the reader could identify the member,
which was never in doubt; it could not confirm what the engine would be
handed.

What works instead is an ink profile over every row band available — bin the
crop region by column and by row, sum dark pixels, and read the gutters off
the histogram. Capture 6's 142 bands put the points column's left edge in a
gutter carrying *zero* ink in all of them, which is a different quality of
claim from "looked right on eight rows." The same profile is what showed the
name crop had been clipping `MoreBallsThanBrains`, a defect nobody had thought
to look for at all.

**It happened a second time, on the roster route, and was found the same way.**
`nameXFrac0` was 0.19 — x=137 of a 720px frame — which is inside the
per-member *status icon* between the avatar and the name, so every member who
has one had a fragment read as their first character: `7 Kun Tsunami`,
`} Lothar232`, `P Ravenna Morrigan`. The constant's own comment recorded that
task 21 had checked it against ten real rows "read back by eye before OCR ever
ran" and concluded the geometry was correct. 56 of capture 1's 331 row bands
were a correct name plus that fragment, against 53 that read exactly. The ink
profile put the gutter at x=156-158 (ink 16 across 68 bands, against 787 at
x=136 and 946 at x=160), and moving the edge there took exact reads from 53 to
141 with junk-prefixed reads to 0.

Two things generalize from the repeat. First, **an eye-check does not become
reliable by being done on a different field** — it failed identically on the VS
name crop and the roster name crop, for the same reason both times. Second,
**the symptom named the wrong culprit**: reading `7 Leroy Jenkins 0914` off a
list of members suggests a rank badge, which is per-group and constant, and the
pixels showed a status icon, which is per-member and optional. That difference
is exactly why only a third of names were affected, and no amount of staring at
OCR output would have settled it.

The corollary applies to anything fitted downstream of a crop: **options
measured through the wrong rectangle are not evidence about the right one.**
`vsNameOptions` skipped thresholding because thresholding destroyed a crop
dominated by a colourful avatar. Once the avatar was out of the crop that
reason was gone — so the obvious move was to turn thresholding back on. Measured
across all eight skip-flag shapes at three upscale factors: it is still worse,
by eight members. Re-measure after moving a crop; do not re-reason.

### Two aggregates side by side are not a causal claim

The M4 spec recorded that "15 row bands read empty" and "this roster has ~10
non-Latin names", and concluded the empty bands *were* the non-Latin names —
so the fix was tesseract language packs. Both aggregates were accurate. The
conclusion was wrong: per member, the empty bands were almost all plain ASCII
(`Drizzlers12`, `2Rule`, `Mc1999`, `Delio1`), and the non-Latin names were
never empty at all — `Danny 狂` read as "Danny 3t", `Guts ツ` as "Guts ‘V". No
language pack would have moved a single empty band.

This is the `worst-in` lesson above reached from another direction: **an
aggregate can be perfectly accurate and still support the wrong story, and
only a per-item view can say which.** `make probe-m4 PROBE_ARGS=-probe.detail`
is the per-item view for the name field; it names each member, its best read,
and whether the read was empty, too weak, or beaten by a rival.

### A broken instrument reports agreement, not noise

The probe's fallback-preprocessing sweep once returned 24 identical rows across
shapes that differ enormously elsewhere. That reads as a clean negative result
("preprocessing doesn't matter here") and is far more persuasive than a bad
number. It was measuring nothing: the retry was happening inside the engine
before the probe ever saw an empty string.

The tell was *implausible* uniformity — `full x2` and `gray x4` cannot produce
identical results on the same crops. Treat suspicious agreement with the same
scrutiny as suspicious disagreement, especially when it confirms that a thing
you did not want to do is unnecessary.

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

### A probe's default is not necessarily production's configuration

`make probe-assign` unions one page-segmentation mode by default; `IngestVS`
reads every name crop at PSM 7 **and** PSM 13 and takes the better per member.
So the bare invocation models a **weaker** pipeline than the one that ships,
and the gap is not cosmetic. Measuring decoration stripping, the default
reported `84/86 correct, wrong 1` — a misattribution, which is the one failure
this project treats as unrecoverable — while `-probe.assignpsm=13` reported
`86/86, wrong 0` on the same change, and `make gate-m4` agreed with the
second. Acting on the default would have rejected a change that is strictly
better.

The general form: an instrument that models the pipeline has a configuration,
and "the default" is a choice someone made for speed, not a claim that it
matches production. Before believing a probe's headline — especially a
`wrong` count, which is the number most likely to stop a change — check that
what it is reading matches what ships. This is the same defect shape as a
documented invocation that silently runs the wrong test; the output is honest
about what it measured and says nothing about what you assumed it measured.

### A passing aggregate hides everything its tolerance is wider than

`make gate-m4` counts a row correct when its parsed points land within 1% of
the hand-checked value. On rank 7 of the M4 capture that tolerance is a window
183,000 wide. `Mar 89` was written as 18,356,304 against a hand-checked
18,356,804 — one digit, an 8 read as a 3 — and the gate counted it among the
rows it passed, because 500 on 18.3M is 0.0027%. Every low-order digit misread
in that capture passes the same way. So the headline (85/86 as of 2026-08-19)
says each row reached the right member carrying roughly the right magnitude; it
does not say the number is correct, and it must not be quoted as if it did.

It was found by dumping the written facts and diffing them against
`expected.yaml` member by member — a different act from running the gate. The
gate's own report cannot surface it by construction, because the row sits on
the passing side of the gate's own arithmetic.

That is the "two aggregates" lesson one step further along. There the
aggregates were accurate and supported the wrong *cause*, and a per-item view
said which cause was real. Here the aggregate is a **pass**: there is no
anomaly to explain and nothing prompts anyone to look, so the per-item view is
what establishes that a failure exists at all. A green number is the hardest
aggregate to interrogate, and interrogating it means a full readout against
ground truth, not a re-run.

### Tesseract's layout analysis is blind to some perfectly legible crops

Not a preprocessing problem and not a contrast problem. A crop can be bold
black text on light grey, correctly framed, with generous margins, and PSM 3,
4, 6, 7, 11 and 12 all return the **empty string**. PSM 8 ("single word") and
PSM 13 ("raw line", which bypasses tesseract's layout hacks) read it. The
failure is in layout analysis, before recognition runs.

Measured over the M4 gate's 142 row bands: 15 read empty at PSM 7 and every
one reads at PSM 13 — 12 of 86 members, unmatchable at any threshold, because
an empty string is not a near miss. But PSM 13 *alone* resolves 31/86 members
against PSM 7's 62/86, so it is only ever a retry, confined to crops that
produced nothing at all.

Three consequences worth keeping:

- **The retry belongs in `ingest`, not in the engine** (`readFieldWithRetry`).
  Whether a poor read beats no read is a property of the field: a name has a
  known roster behind it and a bad read simply fails to match, while a number
  has no such guard and a raw-line retry can *manufacture* a plausible value
  from a crop that caught neighbouring content. Names retry; points do not.
- **The retry's preprocessing is its own measurement.** Inheriting the
  primary's options is an assumption — those were fitted for a mode whose
  layout analysis works. Fitting them for the retry was worth two members
  (`make probe-m4 PROBE_ARGS=-probe.fbsweep`).
- `internal/ocr/testdata/psm7_layout_blind.png` is the whole defect in 2KB,
  and its test is a canary: if PSM 7 ever reads it, the retry can be revisited.

### OCR reads the glyph, not the codepoint

Two consequences that look alike and need opposite fixes.

A **homoglyph** — Cyrillic `о`, Greek `Ο`, a `ł` — is drawn exactly like a
Latin character, so OCR returns the Latin one and has no way not to. The
stored and read forms then share no characters at all, and no threshold helps:
`δkδzδ` and `akaza` are not a near miss, they are disjoint. `roster.Normalize`
folds these, and that is the only reason a name like `ΔKΔŽΔ` is matchable.

A **decoration in another script** — `한씨아저씨`, `Danny 狂`, `٣١٢ A l i ٣١٢` —
is not a homoglyph and must never be folded, because there is nothing to fold
it *to*. An English-only tesseract returns empty for these at every
preprocessing setting (15 of 142 bands, measured).

**Language packs are the obvious fix and were measured inert** on the two rows
the gate actually lost to decoration: `ϟϟ Leo ϟϟ` and `٣١٢ A l i ٣١٢` come back
byte-identical under `+grc`, `+ara` and `+ell` alike. The fix is in the matcher.
`roster.stripDecoration` scores `ϟϟ Leo ϟϟ` against `">> Lea >>"` as `Leo`
against `Lea`, which took the gate 83/86 to 85/86 with every row matched. It
runs **after** homoglyph folding, on `Normalize`'s output, and the reverse order
fails *silently*: the decoration score is taken as a maximum, so stripping raw
text scores near zero, loses the maximum and contributes nothing while every
other test still passes. The pack table and the rest of the reasoning sit above
`vsNameLanguages` in `internal/ingest/vs.go`.

A **confusable** is the third case and needs its own mechanism: the player
typed what the roster records and the *engine* returned a different character,
because the two are drawn alike (`ALBAN80` read as "ALBANSO", `AA91AA` as
"AASIAA"). Unlike a homoglyph it is never folded — the two must stay distinct
keys or two members collapse into one row — so only the *scoring* treats them
as near-equal. `internal/roster/confusable.go` charges 2 tenths of an edit for
a confusable substitution against 10 for anything else, and cheapens
substitution only: a confusable pair is evidence the engine saw the right
glyph and named it wrongly, while an insertion or deletion is evidence it saw
something absent or missed something present.

**Cheapening substitutions spends separation between real members, so show the
budget.** `roster.ClosestPairScore` reports the highest score between any two
distinct names on a roster; `make probe-m4` prints it and fails if it reaches
`AutoAccept`. On the M4 capture it sits at 60 against a threshold of 92. Any
change to `confusableCost` or the pair table is read against that number, not
against the accuracy count alone.

Related, and found while fitting that constant: `ratio()` used to divide a
**rune** distance by a **byte** length, which inflated the score of every
multi-byte name — `한씨아저씨` scored 66 against the unrelated read "AKAZA". It
made false near-misses likeliest on exactly the names hardest to read. If a
score involving non-Latin text looks surprisingly generous, check the units
before the algorithm.

The tempting third response to all of these is lowering `roster.AutoAccept`. Don't.
The threshold is what stops a misread row being attributed to the wrong
member, and that is the one failure mode here a review queue cannot undo: a
queued row is recoverable, one member's score written onto another's row is
not. Raise the score of reads that are genuinely the same name (folding,
confusable-aware distance); do not lower the bar for everything.

## Layout

```
cmd/control     API, scheduler, migrations
cmd/agent       device driver CLI
internal/config env-driven config; malformed values fail loudly
internal/db     schema, embedded migrations, hand-written pgx queries
internal/blob   content-addressed object store (fs + s3 backends)
internal/transport  Transport interface; adb.go, replay.go
internal/capture    screenshot -> blob -> db
internal/runtime    task runtime: Ctx primitives, screen graph, panic route, kill switch
internal/tasks      Tier 1 task skeletons; self-registering catalogue
internal/scheduler  cadence-driven planner + loop; decides what runs when
internal/ingest     capture frames -> facts; OCR, segmentation, reconciliation
internal/roster     name normalization and fuzzy matching to known members
fixtures/       recorded screenshots for device-free tests
```

## Gotchas

- **`adb exec-out`, never `adb shell`, for `screencap`.** `shell` applies CRLF
  translation that corrupts binary PNG output. Exits 0 while doing it.
- **Postgres is on host port 5433**, not 5432 — 5432 is commonly already
  allocated by another project.
- Identical screenshot bytes deduplicate to **one blob but still write a
  separate `screenshots` row**. Each capture is a distinct observation;
  collapsing rows would silently under-report participation.
- A FLAG_SECURE surface returns a **zero-byte capture**, not a black frame.
  On the PIN entry screen `adb exec-out screencap -p` returns 0 bytes;
  backing out returns a full frame again. This fails at PNG decode rather
  than at recognition, which is a different error path from a sleeping
  display and needs its own handling rather than falling through to
  "no screen recognized."
- A black frame is not proof of a sleeping display. A frame captured
  mid-transition is also solid black while `mWakefulness=Awake`,
  `mScreenState=ON` and `mStayOn=true` all report healthy. This weakens the
  blanket reading, not the M2 Phase B diagnosis specifically: Phase B's black
  frame deduplicated onto an object first written days earlier and coincided
  with a device the panic route could show was unresponsive to anything short
  of `Wake`, which is evidence a mid-transition frame doesn't have. The
  conclusion was correct for that evidence; it just no longer generalizes to
  "black frame implies asleep" on its own.
- **A gate that reads the blob store must be given an absolute
  `LW_BLOB_FS_ROOT`.** The fs backend defaults to the relative `./data/blobs`
  and `go test` runs each package binary in *its own source directory*, so a
  test in `internal/ingest` looks under `internal/ingest/data/blobs` and finds
  nothing. It skips reporting a missing frame, which reads as a bad capture
  rather than a mislocated store. `make gate-m4` defaults it with `?=`.
- **A swipe's fling is not always finished when the settle expires.** The
  900-1400ms `swipeSettle` is usually enough and occasionally is not: a real
  `vs_capture` frame was screenshotted while the list was still decelerating,
  and the list moved a further 25px afterwards with no input at all. A
  mid-fling frame does not fail obviously — it inverts the thinnest probe's
  lattice margin in `vision.ScrollOffset`, which then refuses (correctly)
  rather than returning a wrong offset. The fix is another screenshot, never
  another swipe: swiping again advances the list a second time and skips every
  row in between.
- **Rank groups have no fixed identity.** Group names are user-editable, the
  group set itself varies — there was no R4 group three weeks before there
  was one — and the rank badges differ from one another by a single digit,
  so nothing about a group is safe to key on. `roster_capture` therefore
  never asks which group it is looking at: it opens whichever
  `chevron_collapsed` anchor `Match` finds next and stops once none remain,
  and rank attribution happens at ingest, from OCR of each frame's own
  sticky header, rather than a label the task would otherwise have to
  assert. This is the clearest case in the project of something a single
  capture session cannot reveal — one session shows a self-consistent world,
  and only two sessions weeks apart show what actually moves, which is the
  entire reason this project exists.

## Operational reality

Automation of this kind violates Last War's ToS and accounts can be banned.
Run alts, not a main. Humanize timing. Keep the kill switch working.

### The handset must not sleep, and must not have a keyguard

```bash
adb shell svc power stayon true              # necessary, not sufficient — see below
adb shell dumpsys power | grep -o 'mStayOn=[a-z]*'   # verify: want true
```

This is setup, not code, and the runtime cannot enforce it. A sleeping display
is **indistinguishable from a broken one**: `screencap` returns an all-black
frame, which matches no anchor, so recognition fails exactly as it would on a
corrupted screen. The M2 24-hour run lost four hours to this and the panic
route recovered 0 of 10 incidents, because neither a back press nor an app
restart turns a screen on — the ladder was retrying the only three things that
cannot help.

`Transport.Wake` is the panic route's rung zero and does recover a dozing
display — confirmed on the handset: `panic route: recovered by waking the
display screen=base`, the first successful recovery this route has ever
recorded.

**`stayon` is necessary but not sufficient on this handset, and the mechanism
of the gap is unknown.** M4 found the phone dozing behind a keyguard with
`mStayOn=true`, `stay_on_while_plugged_in=15` on all four sources, USB
powered, battery status FULL at 100%, thirteen days of uptime with no reboot,
and `RUNNING_UNLOCKED` — a re-lock, not a lock left over from a boot `stayon`
never reached. It then dozed twice more over the course of the milestone,
each time with `mStayOn=true` still reporting healthy. Per the discipline the
radar fix established, the response to that cannot wait on understanding why:
`Wake` turns the display on and lands directly on the keyguard, where a
capture doesn't come back black, it comes back **zero bytes** (see Gotchas) —
so **clearing the keyguard credential is mandatory, not advisory**. `stayon`
is not a substitute for that on this device, only a reduction in how often the
gap gets exercised, since the display still re-sleeps during the 90s restart
wait.

**A keyguard defeats all of it, and adb cannot clear a secured one.**
`locksettings set-disabled true` reports success and changes nothing when a PIN
is set — `locksettings get-disabled` still returns `false` afterwards. Nor will
`KEYCODE_MENU`, `wm dismiss-keyguard`, a swipe or a power toggle: all leave
`isKeyguardShowing=true`. Unlocking needs the credential:

```bash
adb shell input swipe 360 1400 360 400 200   # x=360: this panel is 720 wide
adb shell input text <pin>
adb shell input keyevent KEYCODE_ENTER
```

Which an unattended run cannot do. So the PIN is cleared on the automation
handset (`locksettings clear --old <pin>`) before any unattended run — not
offered as a fallback to `stayon`, since `stayon` reporting `true` has already
been observed next to a locked screen. Check `mStayOn` anyway before starting
an unattended run: a dozing-but-unlocked display is still a real failure mode
and `Wake` still recovers it — discovering either one four hours in costs the
run.
