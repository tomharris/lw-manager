# lw-manager

Automation and alliance analytics for **Last War: Survival**. One system that
drives alternate accounts on real Android hardware and turns what those
accounts can see into per-member participation data you own.

## The thesis

Two halves of this problem already have solutions, and each lacks what the
other has. Cloud bots automate tasks but surface no alliance data. The
open-source alliance managers track rosters and VS scores but need somebody to
upload screenshots by hand, every day, forever.

**The bot is the collection tier for the analytics tier.** An agent that can
navigate to the alliance roster and to the VS ranking can screenshot them on a
schedule and feed the parser directly. That closes the manual-upload loop, and
it is the reason to build one system instead of gluing two together.

Everything is screen-in / touch-out over adb — no packet interception, no
client patching. Slower than the alternatives, and it keeps the observable
surface area at "a fast player."

## Status

| Milestone | | |
|---|---|---|
| **M0** Foundations | ✅ | Postgres, migrations, config, `Transport`, capture → blob store |
| **M1** Vision core | ✅ | **99.53%** recognizer accuracy (632/635) against a 98% gate. All three misses are missed detections, not misidentifications — the confusion matrix has no false positives at all, which is the property invariant #3 depends on: a screen the recognizer declines to name is a task that declines to act. |
| **M2** Task runtime | ✅ | Runtime, screen graph, panic route, kill switch, scheduler, and all five Tier 1 task bodies. Gated on the handset over 24 unattended hours: **95.82%** (229/239) against a 95% bar, zero stuck runs, and invariant #3 held through a four-hour display outage. The three defects that run exposed — a panic route that recovered nothing, a scheduler planning in UTC, and `radar`'s wrong flow model — are fixed since; `mail_collect` still has no successful unattended observation. |
| **M4** Analytics collection | 🚧 | Both routes run end to end on the handset: scroll-and-stitch with measured offsets, phase-locked row segmentation, per-field OCR (plus NCC for the rank badges no OCR can read), append-only facts, and uncertain reads triaged in `agent studio`. Against a real 86-row capture the gate now **passes at 85/86 (98.84%)** against its 95% bar, up from 63/86: every row matched to a member, one queued for review, nothing dropped silently, and the capture still reconciles. The gain came from matching the whole ranking as a **closed set** — one assignment over all rows and all members at once — rather than 86 independent threshold lookups, plus points bounded by the ranking's own order and decoration-stripping in the matcher. Language packs were the obvious fix for the two decorated names and were measured inert. Three instruments are committed with it: `make probe-m4`, `make probe-points`, `make probe-assign`, the last of which self-checks with a shuffled-truth canary and decoy padding. Measured in full in `docs/superpowers/specs/2026-08-17-m4-closed-set-matching-design.md`. |
| **M5** Participation surface | ⬜ | Leaderboard, VS compliance, inactivity watchlist |
| **M3** Fleet | 💤 | Deferred until after M5, and may not happen. Dashboard + WebSocket status; the registry and the multi-device run loop already shipped in M0/M2 |

The M1 corpus is 635 frames across 15 screens with 55 negatives, captured from
a moto g play 2024 at 720×1600 on game versions 1.0.354 and 1.0.358.

## Quickstart

```bash
docker compose up -d          # postgres on :5433, minio on :9000
make build                    # bin/agent, bin/control
./bin/control migrate
```

With a device listed by `adb devices`:

```bash
./bin/agent devices                                    # confirm the probe works
./bin/agent register --nickname myalt --role alliance_data
./bin/agent capture --account <id printed above>
./bin/agent run                                        # scheduler loop
./bin/control pause --reason "alliance event"          # global kill switch
./bin/control resume
```

`register` probes the device over adb rather than taking a resolution flag —
registration is the one cheap moment to prove the serial is real and reachable.
It is idempotent, so re-running it corrects a typo instead of creating a
duplicate.

Turning captures into facts is a separate, idempotent pass:

```bash
./bin/control alliance set --tag <tag> --name <name>   # the one tracked alliance, once
./bin/control ingest --capture <id>                    # roster or VS ranking, decided by the capture
```

### Vision tooling

```bash
./bin/agent record --interval 2s --duration 10m   # burst-capture frames
./bin/agent studio --addr 0.0.0.0:8088            # label and crop, in a browser
./bin/agent corpus index && ./bin/agent corpus push
./bin/agent score                                 # accuracy, confusion matrix, separation report
./bin/agent score --json                          # + per-frame predictions
make gate                                         # the same gate, as a test
```

Labelling is a served UI rather than a file manager because the build host is
headless: over SSH, "sort the screenshots into folders" means `mv`-ing 200
files you cannot see. Threshold tuning deliberately stayed on the CLI — the
separating number is computed across the whole corpus, not eyeballed.

## How it works

**Transport** owns pixels. Everything above it speaks normalized `[0,1]`
coordinates, and `Norm.Pixels` is the only place they become integers. Swapping
a 720×1600 handset for a different device changes one layer.

**Vision** is a hand-rolled NCC template matcher plus a screen recognizer that
aggregates per-screen anchors by minimum — so one weak anchor caps its whole
screen, which is why the separation report keys on *(anchor, screen)* rather
than on anchor alone.

**Runtime** gives task code a deliberately small surface: navigate, tap, swipe,
wait, sleep. There is no accessor for the underlying transport, and `Tap`
accepts an anchor ID rather than a coordinate — the no-blind-taps invariant is
enforced by the signature, not by review.

**Ingest** turns captured frames into append-only facts: rows segmented on a
fitted phase rather than a fixed pitch, each field read through its own
preprocessing, and the VS ranking matched to the roster as a **closed set** —
every row against every member in one assignment, so a name that is ambiguous
on its own is resolved by which member is still unclaimed. Every number carries
a confidence and a screenshot reference, and anything under the bar goes to a
review queue rather than to a leaderboard. Misattributing one member's score to
another is the one failure a queue cannot undo, which is why the threshold never
moves to buy accuracy.

**Scheduler** derives each account's offline window and plans from cadence,
weekday, and role. The kill switch is checked between every step, globally and
per account.

## Layout

```
cmd/control          API, scheduler, migrations
cmd/agent            device driver CLI
internal/config      env-driven config; malformed values fail loudly
internal/db          schema, embedded migrations, hand-written pgx queries
internal/blob        content-addressed object store (fs + s3)
internal/transport   Transport interface; adb.go, replay.go
internal/capture     screenshot → blob → db
internal/runtime     Ctx primitives, screen graph, panic route, kill switch
internal/tasks       task catalogue; self-registering
internal/scheduler   cadence-driven planner and loop
internal/ingest      capture frames → facts; OCR, segmentation, reconciliation
internal/roster      name normalization and fuzzy matching to known members
internal/vision      matcher, recognizer, corpus scoring
internal/corpus      record, index, push/pull of the fixture corpus
internal/studio      served labelling and cropping UI
internal/ocr         tesseract CLI wrapper
templates/           anchor images + manifest.yaml
fixtures/corpus/     index.yaml only; the bytes live in the blob store
```

## Testing

```bash
make test              # unit. Passes with nothing running — no device, no Docker
make test-integration  # needs docker compose up; runs against lw_manager_test
make test-device       # needs a device on adb; skips without one
make gate              # M1 recognizer accuracy against the real corpus
make gate-m4           # M4 ingest against a hand-checked 86-row capture
```

The three M4 probes are **measuring instruments, not gates** — they assert
nothing and always pass, and their output is the point:

```bash
make probe-m4          # the VS name field, scored against 86 hand-transcribed names
make probe-points      # the points field: exact, within 1%, unparseable, retried
make probe-assign      # closed-set matching, with a shuffled-truth canary and decoys
```

Reach for one before changing a crop, a preprocessing option, a
page-segmentation mode or a matcher constant — and again afterwards. A crop
"verified by eye" against a handful of rows is not measured: all three original
M4 crops passed that review and scored 0 of 86 rows on the first real capture.

Integration tests read `LW_TEST_DATABASE_URL`, never the application's
`LW_DATABASE_URL`, and `internal/dbtest` refuses any database not named
`*_test`. The guard, not the default, is what keeps a developer with the app's
variable exported from pointing the suite at real data.

The corpus itself is not in git — 635 full-resolution screenshots is several
hundred megabytes that git would keep forever. Only `fixtures/corpus/index.yaml`
is tracked; `agent corpus pull` materializes the rest.

## Conventions

`CGO_ENABLED=0`, always — enforced by `make verify-nocgo`. That rules out gocv,
gosseract, and onnxruntime, and it is a deliberate trade: OCR shells out to the
`tesseract` CLI and template matching is hand-rolled.

The non-negotiable invariants — normalized coordinates, idempotent tasks, no
blind taps, append-only facts, confidence on every OCR read, device-free tests,
jittered sleeps, and the kill switch — are documented in
[`CLAUDE.md`](CLAUDE.md). The full design is in
[`docs/lastwar-platform-design.gen`](docs/lastwar-platform-design.gen).

## Operational reality

Automation of this kind violates Last War's Terms of Service and accounts using
it can be banned. Run alts, never a main. Humanize timing. Keep the kill switch
working. This is a personal project for an alliance the author is in; it is not
a service, and nobody's credentials leave the box.
