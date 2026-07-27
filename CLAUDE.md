# lw-manager — conventions and invariants

Last War automation and alliance analytics platform. The architectural thesis:
**the bot is the collection tier for the analytics tier.** A bot that can
navigate to the alliance roster and to the VS ranking can screenshot those
screens on a schedule and feed the parser directly.

Those are two separate routes, not one chain. The roster is Alliance →
Members. The VS ranking is **not reachable from Alliance at all**: it is
base → VS → Ranking → "weekly ranking" tab → select your alliance. Earlier
docs describe it as "Alliance → Members → VS Ranking", which is wrong and is
worth correcting wherever it appears — a route that does not exist is the
kind of thing a task gets written against.

Full design: `docs/lastwar-platform-design.gen`.
Current milestone spec: `docs/superpowers/specs/`.

## Quickstart

```bash
docker compose up -d          # postgres on :5433, minio on :9000
make build                    # bin/agent, bin/control
./bin/control migrate

# with an emulator running and `adb devices` listing it:
./bin/agent devices                                  # confirm the probe works
./bin/agent register --nickname myalt --role alliance_data
./bin/agent capture --account <id printed above>
./bin/agent run-task --account <id> --task help_all   # runs once templates exist
./bin/agent run                                       # scheduler loop, all attached devices
./bin/control pause --reason "alliance event"         # global kill switch
./bin/control resume
./bin/agent accounts                                 # what is registered

./bin/agent record --interval 2s --duration 10m   # burst-capture the corpus
./bin/agent studio --addr 0.0.0.0:8088            # label and crop, from a browser
./bin/agent corpus index && ./bin/agent corpus push
./bin/agent score                                 # the M1 gate, with diagnostics
make gate                                         # the same gate, as a test
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
- `make gate` — the M1 phase gate: recognizer accuracy against the real
  corpus. Tagged `//go:build corpus`, device-free but slow, so it stays out
  of `make test`. Skips when the corpus has not been pulled.
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

200+ full-resolution screenshots is 300–600 MB, which git would keep in
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

The recognizer needs an identifying anchor for **every** labeled screen, not
just the six `DefaultGraph()` navigates. `alliance_members` and `vs_ranking`
are in the corpus for M4; without anchors they would be wrong on every
scoring run forever. Recognition and navigation are separate concerns.

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

## Operational reality

Automation of this kind violates Last War's ToS and accounts can be banned.
Run alts, not a main. Humanize timing. Keep the kill switch working.
