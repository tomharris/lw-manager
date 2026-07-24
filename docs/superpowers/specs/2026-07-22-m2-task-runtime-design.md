# M2 — Task Runtime (device-free slice)

## Context

M1's vision core is complete in its device-free half: template registry,
scale-invariant NCC matcher, screen recognizer, and the OCR interface all ship
with synthetic-fixture tests. What remains of M1 — the 200+ screenshot corpus
and the ≥98% recognition gate — is blocked on hardware (the game
fingerprint-blocks login on the emulator; a physical phone is on the way).

Rather than wait, this slice builds the **M2 task runtime**: the layer that
turns recognized screens into executed tasks. It is device-free by
construction — `ReplayTransport` exists precisely so runtime logic can be
built and tested before hardware — and it is where three of the platform's
invariants become code:

- **#3** No task acts without a matched screen anchor first.
- **#7** Sleeps go through the jittered context helper.
- **#8** The kill switch is checked between every task step.

The design principle throughout: make these invariants **structurally
unavoidable**. The task author has no method available that can violate them.

### Scope decisions made during design

| Decision | Choice | Rationale |
|---|---|---|
| Which tier next | M2 task runtime, not ingest/analytics | Keeps milestone order; runtime is the riskiest untested layer and both #7 and #8 live here |
| How far into M2 | Runtime primitives **plus all five Tier 1 task skeletons** | Skeletons prove the primitives compose; accepted risk that skeletons written against expected screens get revised when the real corpus lands |
| Scheduler | **Deferred** to a follow-up slice | Tasks run on demand via `agent run-task`; the runtime API should settle before a scheduler consumes it |
| Task expression | Imperative Go functions over a constrained `runtime.Ctx` | Chosen over an explicit FSM (verbose in Go; linear tasks read worse as transition tables) and over declarative step lists (scroll-until-done loops become a worse language embedded in data). Resumability comes from idempotence-by-observation, not persisted FSM state |
| M2 gate | **Deferred to hardware** | The 24h-unattended / ≥95%-success gate needs a real device, same as M1's corpus gate. This slice's exit is: all runtime code and skeleton tests green with nothing attached |

---

## Layout

```
internal/runtime      Ctx (constrained primitives), screen graph, panic
                      route, kill switch, jittered sleep
internal/tasks        one file per task; self-registering registry
internal/db           migration 00002: task_runs + flags tables
internal/transport    add Back(ctx) to the Transport interface
cmd/agent             new: run-task --account <id> --task <name>
cmd/control           new: pause --reason <r> / resume
```

Data flow for one run: `agent run-task` loads registry + graph → validates
the graph against the registry (loud failure at startup) → opens transport →
builds `runtime.Ctx` → executes the task function → records a `task_runs`
row with status, error, and captured screenshot IDs.

## Transport change: `Back`

The panic route's cheapest recovery is spamming Android's back button, which
works precisely when the current screen is *unknown* — the panic route's whole
situation — so it cannot be modeled as an anchor tap. One method is added to
`transport.Transport`:

```go
// Back presses the Android back button. The panic route's first resort.
Back(ctx context.Context) error
```

`ADBTransport` implements it as `input keyevent KEYCODE_BACK` (via `exec-out`
conventions already in adb.go). `ReplayTransport` records Back calls alongside
taps so panic-route tests can assert the exact recovery sequence.

## `runtime.Ctx` — the constrained primitives

A task is `func Run(ctx context.Context, rt *runtime.Ctx) error`. `Ctx` wraps
transport, recognizer, kill switch, and the capture pipeline, and exposes the
**only** verbs available to task code:

```go
rt.WaitFor(ctx, screen string) (Recognition, error) // poll until recognized, bounded
rt.CurrentScreen(ctx) (Recognition, error)          // one-shot recognize
rt.Tap(ctx, screen, anchorID string) error          // verify screen, match anchor, tap jittered point in bbox
rt.Swipe(ctx, from, to transport.Norm) error        // jittered duration and endpoints
rt.TypeText(ctx, s string) error
rt.Sleep(ctx, min, max time.Duration) error         // jittered, context-cancellable
rt.NavigateTo(ctx, screen string) error             // graph walk from wherever we are
rt.Capture(ctx, screenID string) (int64, error)     // screenshot → blob → db row
```

Structural enforcement:

- **Every primitive begins with a kill-switch check** and returns `ErrPaused`
  or `ErrAccountDisabled` (sentinels, compared with `errors.Is`), so a running
  task unwinds within one primitive of the flag flipping.
- **`Tap` accepts no coordinates.** It takes `(screen, anchorID)`, and
  internally: screenshot → recognize → confirm still on `screen` → match the
  anchor → tap a jittered point inside the match's bounding box. Invariant #3
  cannot be violated because the API has no blind-tap shape. The tap point is
  jittered within the central region of the bbox, never dead center — fixed
  tap positions are as detectable as fixed timing.
- **`Swipe` and `TypeText` verify the current screen is recognized** before
  acting — invariant #3 covers every action, not just taps. Swipe endpoints
  are normalized points because scrolling is positional by nature, but a
  swipe on an unrecognized screen is still a blind action and is refused.
- **`Sleep` is jittered by construction** and respects context cancellation.
  There is no fixed-duration sleep in the API.
- **There is no `rt.Transport()` escape hatch.**

`Recognition` carries `{Screen string, Confidence float64}`.

## Screen graph and `NavigateTo`

The graph is data: nodes are screen names from the M1 registry; edges are
`(from, to, action)` where an action is `tap <anchorID>` or `back`. It is
defined as a Go literal table in `internal/runtime/graph.go` — not YAML —
because edges must be validated against the loaded registry anyway, and a Go
table makes action kinds compile-time checked.

Validation at `Ctx` construction, failing loudly (the config package's
pattern): every edge's screens exist in the registry; every tapped anchor
exists on its `from` screen. Skeleton tasks reference screens whose templates
do not exist yet, so they **refuse to run** until the corpus lands — correct
behavior, not a gap.

`NavigateTo(target)`: recognize current screen → BFS shortest path → execute
each edge (each hop is a `Tap`/`Back` plus `WaitFor(next)`, inheriting
kill-switch checks and anchor verification) → re-verify arrival. A hop that
lands somewhere unexpected but *recognized* re-plans from there — free
resilience against popups that eat one tap.

## Panic route

Triggered inside `Ctx` on `ErrNoScreenRecognized` after a primitive's internal
retry budget. Task code never sees it; recovery is attempted before any error
reaches the task:

1. Press `Back`, wait (jittered), re-recognize — up to 3 times. Popups and
   interstitials die to Back.
2. `AppRestart`, then wait for the app's known entry screen on a long timeout.
3. Still unrecognized → return `ErrLost` (sentinel). The run is marked failed
   with the last screenshot attached, and the agent stops rather than flails.

A task interrupted by recovery is simply re-run from the top later: tasks are
idempotent-by-observation (invariant #2), skipping work the screen state shows
is already done.

## Kill switch

Migration 00002 adds a one-row `flags` table:

```sql
CREATE TABLE flags (
    id         boolean PRIMARY KEY DEFAULT true CHECK (id),  -- single row
    pause_all  boolean     NOT NULL DEFAULT false,
    reason     text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

In `runtime`:

```go
type KillSwitch interface {
    Check(ctx context.Context) error // nil | ErrPaused | ErrAccountDisabled
}
```

The DB implementation reads the flag row and the account's `enabled` column in
one query, cached for ~2s so a tight tap sequence does not hammer Postgres —
well inside the five-second stop requirement. `./bin/control pause --reason
"alliance event"` and `resume` flip the row. The REST endpoint comes with the
API milestone. Tests use a fake switch flipped mid-task to prove unwinding.

## Tasks and `task_runs`

Each task is one file in `internal/tasks`, self-registering in `func init()`
into a name→func map (the `database/sql` driver pattern). Five Tier 1
skeletons: `daily_gather`, `help_all`, `mail_collect`, `tech_donate`,
`radar` — written against expected screen names and anchors, each with a
synthetic-fixture test proving its logic composes.

Migration 00002 also adds:

```sql
CREATE TABLE task_runs (
    id             bigserial PRIMARY KEY,
    account_id     bigint NOT NULL REFERENCES accounts(id),
    task_name      text NOT NULL,
    started_at     timestamptz NOT NULL DEFAULT now(),
    ended_at       timestamptz,
    status         text NOT NULL DEFAULT 'running', -- running|succeeded|failed|paused
    error          text,
    screenshot_ids bigint[] NOT NULL DEFAULT '{}'
);
```

The `running` row is written before the task starts and updated at the end, so
a killed process leaves a visibly stale `running` row rather than nothing —
the audit trail half of invariant #2.

## Testing

All device-free (invariant #6): `ReplayTransport` frame scripts plus synthetic
screens generated the same way the M1 matcher tests generate theirs. Store
fakes for unit tests; real Postgres behind the `integration` tag via `dbtest`.

Key scenarios:

- Multi-hop `NavigateTo` with re-planning after a surprise-but-recognized
  "popup" frame.
- Panic route: Back-recovery succeeds; Back fails → restart recovers; both
  fail → `ErrLost`, run marked failed.
- Kill switch flipped mid-task: task unwinds within one primitive,
  run status `paused`.
- Each Tier 1 skeleton end-to-end against a scripted frame sequence.
- `run-task` writes correct `task_runs` rows for success, failure, and pause.
- Graph validation rejects edges naming unknown screens or anchors.

## Deferred

- Scheduler (follow-up slice, control-side).
- The M2 gate (24h unattended, ≥95% success, zero stuck screens) — needs the
  phone, like M1's corpus gate.
- REST `POST /fleet/pause` — with the API milestone.
- Real anchor templates for the five task screens — from the corpus capture.
