# M0 — Foundations & Capture Path

## Context

`docs/lastwar-platform-design.gen` specifies a unified Last War platform where **the bot is the collection tier for the analytics tier**: an automation agent that already navigates to Alliance → Members → VS Ranking can screenshot those screens on a schedule and feed a parser directly, closing the manual-upload loop that limits `vervelak/lastwar-alliance-manager`.

The repo is currently empty apart from that design doc. This plan covers **M0 only** — the foundations and the screenshot capture path. The doc spans four independent subsystems (device automation, vision/OCR, fleet control, alliance analytics); each later milestone gets its own design cycle.

M0's job is narrow but load-bearing: stand up the Go monorepo, the database, the object store, and the `Transport` abstraction, then prove the capture path end to end. Everything downstream depends on the **fixture corpus**, and the fixture corpus cannot be built until capture works. That is why capture is first.

### Decisions made during design

| Decision | Choice | Rationale |
|---|---|---|
| Backend language | **Pure Go, no CGO** | Single static binary, trivial cross-compile and deploy |
| Frontend | React/Next.js + TypeScript | User preference; deferred past M0 (see Deferred) |
| Device plane | **Android emulator on this Linux box** | `adb` is identical to physical devices, so `AdbTransport` is unchanged if hardware arrives later |
| OCR (M1) | `OCREngine` interface, Tesseract-via-subprocess first | Only CGO-free OCR path. ONNX/PaddleOCR *also* requires CGO (`onnxruntime_go` wraps the C lib), so "pure Go + PaddleOCR" is impossible. Measure Tesseract against fixtures at the M1 gate before paying for anything more |
| Template matching (M1) | Hand-rolled NCC in pure Go | No pure-Go NCC library exists; gocv would reintroduce CGO plus all of OpenCV. Feasible because anchors are small templates in restricted regions, not full-frame multi-scale search |

### Corrections to the design doc

Two of the doc's premises are wrong and shaped these choices:

1. **`vervelak/lastwar-alliance-manager` is a Go project**, not Python — Go 1.23, gorilla/mux, SQLite, gosseract, vanilla JS. The analytics tier has a Go reference implementation to lift patterns from. Pure Go is far less of a fight than the doc implies.
2. **The doc's Python code samples are illustrative, not portable.** There is no `~/lastwar-bot/` on this machine (it was written for a Mac), so "port the existing matcher" has no source. All vision code is greenfield.

Its OCR recipe, from `IMAGE_RECOGNITION.md`, is worth carrying over verbatim in M1 — accuracy there came from **preprocessing and validation, not the engine**: per-row `PSM_SINGLE_LINE`, crop → grayscale → histogram equalization → adaptive threshold (25px block, 15px dense rows) → invert → 3× nearest-neighbour upscale; Sobel-gradient row boundaries; validation gates (power ≥ 1,000,000, no newlines in names).

---

## Repo layout

Go monorepo, module `github.com/tomharris/lw-manager`, adapted from doc §8 to Go conventions (`cmd/` + `internal/` rather than top-level packages):

```
lw-manager/
├── CLAUDE.md                  # invariants (see below)
├── go.mod                     # github.com/tomharris/lw-manager
├── docker-compose.yml         # postgres, minio
├── Makefile                   # test, lint, migrate, up
├── cmd/
│   ├── control/main.go        # M0: migrations + health endpoint only
│   └── agent/main.go          # M0: `agent capture --device X --account N`
├── internal/
│   ├── config/                # env-driven config, no secrets in repo
│   ├── logging/               # log/slog handler setup
│   ├── db/
│   │   ├── migrations/        # goose .sql migrations
│   │   └── queries/           # sqlc source; generated code alongside
│   ├── blob/                  # Blobstore iface: fs.go, s3.go
│   ├── transport/             # Transport iface: adb.go, replay.go
│   └── capture/               # orchestrates transport → blob → db
├── fixtures/                  # recorded screenshots for device-free tests
└── docs/lastwar-platform-design.gen
```

`web/` (Next.js) is intentionally absent until there is an API worth consuming.

## Core interfaces

**`internal/transport`** — the doc's Python `Transport` Protocol, in Go. Returns `image.Image` rather than a numpy array, and takes **normalized** coordinates so invariant #1 is enforced by the type system rather than by discipline:

```go
type Norm struct{ X, Y float64 } // both in [0,1]

type Transport interface {
    Screenshot(ctx context.Context) (image.Image, error)
    Tap(ctx context.Context, p Norm) error
    Swipe(ctx context.Context, from, to Norm, d time.Duration) error
    TypeText(ctx context.Context, s string) error
    AppRestart(ctx context.Context) error
    Resolution() image.Point
}
```

Denormalization to pixels happens **only** inside an implementation — the single place absolute coordinates are legal.

- `adb.go` — `exec.Command` wrapper. Screenshot via `adb -s <serial> exec-out screencap -p` piped into `image/png`. `exec-out` is required over `shell`, which mangles binary output with CRLF translation on many devices. All device state (serial, resolution) probed once at construction via `wm size`.
- `replay.go` — serves PNGs from a fixture directory in deterministic order; records taps/swipes to an in-memory log for assertions. This is what makes every downstream test runnable with no device attached, and it is written in M0 precisely so M1's vision work inherits it.

**`internal/blob`** — `Put(ctx, key, r) error` / `Get(ctx, key) (io.ReadCloser, error)`. `FSBlobstore` backs tests and local dev; `S3Blobstore` (minio-go, pure Go) backs compose. Keys are content-addressed by SHA-256, so re-capturing an identical screen costs no storage and the hash doubles as the dedupe key.

## Database

Postgres via `jackc/pgx/v5` (pure Go), migrations via `pressly/goose`, typed queries via `sqlc`. M0 creates only the four tables the capture path touches — the rest of doc §6 lands with the milestone that needs it:

```
devices(id, serial, transport, resolution_w, resolution_h, status, last_heartbeat)
app_instances(id, device_id, package, clone_id)
accounts(id, app_instance_id, game_uid, nickname, server, role, alliance_id, enabled)
screenshots(id, account_id, captured_at, screen_id, object_key, sha256)
```

`screenshots.screen_id` is nullable in M0 — nothing recognizes screens yet. M1 populates it.

## Capture path

`internal/capture` is the one place these pieces meet, and it stays deliberately thin:

1. Resolve account → app_instance → device; construct the device's `Transport`
2. `Screenshot()` → encode PNG → SHA-256
3. `blob.Put` under a content-addressed key (skip write if the key already exists)
4. Insert `screenshots` row inside a transaction
5. Return the row ID

Exposed as `agent capture --device <serial> --account <id>`, which is the M0 gate.

## CLAUDE.md

Ships in M0 with the doc §11 invariants translated to Go:

1. No absolute pixel coordinates outside a `Transport` implementation. `Norm` only.
2. Every task is idempotent and interruptible.
3. No task acts without a matched screen anchor. Blind taps are a bug.
4. Facts are append-only. Corrections supersede; nothing is mutated in place.
5. Every OCR-derived number carries a confidence and a screenshot reference.
6. All vision logic ships with fixture-based tests that run with no device attached.
7. Sleeps go through the jittered context helper. Never bare `time.Sleep`.
8. The kill switch is checked between every task step.

Plus Go-specific conventions: `CGO_ENABLED=0` is enforced in the Makefile and CI, `context.Context` first arg everywhere, errors wrapped with `%w`, `log/slog` for all output.

---

## Build sequence

Test-driven throughout — `ReplayTransport` and `FSBlobstore` mean nearly all of M0 is testable without a device or Docker.

1. `go mod init github.com/tomharris/lw-manager`; Makefile with `CGO_ENABLED=0`; `internal/config` + `internal/logging`
2. `docker-compose.yml` (postgres, minio); goose migrations for the four tables; sqlc config and generated queries
3. `internal/blob` — interface, `FSBlobstore`, `S3Blobstore`, tests
4. `internal/transport` — `Transport`, `Norm`, `ReplayTransport` **first** (tests come free), then `AdbTransport`
5. `internal/capture` — capture service, tested end to end against `ReplayTransport` + `FSBlobstore`
6. `cmd/agent` capture command; `cmd/control` health endpoint + migration runner
7. `CLAUDE.md`

## Prerequisites (user action)

- Install `adb` (`android-tools-adb` / platform-tools)
- Stand up the Android emulator (Waydroid or Android Studio AVD) and confirm `adb devices` lists it
- Install Last War on it and sign in to an alt account

Steps 1–5 can proceed before any of this; only the `AdbTransport` integration test and the M0 gate need a live device.

## Verification

**M0 gate:** `agent capture --device <serial> --account 1` writes both a blob and a `screenshots` row, and the stored PNG opens and shows the live game screen.

- `CGO_ENABLED=0 go build ./...` succeeds — proves the no-CGO constraint holds
- `go test ./...` passes **with no device attached and no emulator running** — proves the replay/fixture discipline is real
- `docker compose up -d` then `go test -tags=integration ./...` for Postgres/MinIO-backed tests
- Capture the same screen twice; confirm one blob, two `screenshots` rows (content-addressing works)
- `psql` the `screenshots` row and confirm `object_key` resolves in MinIO with a matching SHA-256
- Manual: `AdbTransport.Tap` with a known `Norm` moves the game as expected on the emulator

## Deferred (later cycles)

- **M1 vision** — NCC matcher, template registry, screen recognizer, `OCREngine` + Tesseract subprocess, vervelak's preprocessing pipeline. Requires a 200+ screenshot fixture corpus captured first.
- **Frontend** — Next.js/React/TS. Worth flagging: doc §7 explicitly recommends *against* a heavy SPA (suggesting htmx or SvelteKit), on the grounds that the review queue is mobile-first. Next.js is the choice here, but that tension should be revisited when the dashboard is designed.
- **M2+** — task FSM runtime, scheduler, fleet orchestration, analytics collection routes, MCP server.
