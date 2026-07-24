# Scheduler — cadence-driven task execution (device-free slice)

## Context

M2 built the task runtime: `runtime.Ctx` primitives, the screen graph, the
panic route, the kill switch, and five Tier 1 task skeletons, all runnable
on demand via `agent run-task`. The scheduler was deliberately deferred so
the runtime API could settle before anything consumed it. It has, so this
slice builds the consumer.

Everything here is device-free-testable. Deciding *what should run now* is
pure logic over data already in Postgres: `task_runs` records when each task
last ran per account, `flags` + `accounts.enabled` is the kill switch, and a
new `tasks` table supplies cadences. Only execution needs the phone, and
execution sits behind an interface that tests fake — the same "fake before
real" ordering that produced `ReplayTransport` before `ADBTransport`.

### Scope decisions made during design

| Decision | Choice | Rationale |
|---|---|---|
| Scheduling model | Cadence + daily offline window | The design doc's full `(priority, cooldown, allowed_window, energy_cost)` queue needs live game state we cannot read device-free; energy and preconditions would be stubs rewritten when the phone lands |
| Schedule config | DB `tasks` table with role matching | Matches the reserved data model (doc §6), editable without redeploy, gives M3's per-account overrides somewhere to hang |
| Loop host | Host-agnostic library, wired as `agent run` | The device lives behind adb on the agent, and no control→agent dispatch channel exists — building one pulls M3 fleet work into this slice |
| Offline window | Derived from `hash(account_id, date)` | No storage or assignment job; a restart recomputes the same window instead of granting fresh eligibility |
| Engine shape | Pure `Plan` function + thin I/O loop | Matches how this codebase already isolates decisions (`scoreScreen`, `Graph.Path`, `jitter`); recomputing from durable state each tick satisfies invariant #2 |
| Priority ordering | Longest-overdue first | Fair and starvation-free without a hand-tuned `priority` column |

---

## Layout

```
internal/scheduler
  schedule.go   Task, Account, Snapshot, Decision types; Plan (pure)
  window.go     OfflineWindow derivation (pure)
  backoff.go    consecutive-failure backoff (pure)
  loop.go       Loop: snapshot → Plan → execute → sleep; Store and Executor ifaces
internal/db     migration 00003 (tasks table); SchedulerSnapshot query
cmd/agent       new: `agent run` — the scheduler loop against the local device
```

The library knows nothing about devices, adb, or `internal/runtime`. It
depends on two interfaces: `Store` (read a snapshot) and `Executor` (run one
task for one account). Production wires `Executor` to `runtime.Run` over an
`ADBTransport`; tests wire a recorder. When M3 moves orchestration
control-side, the same package is re-hosted behind a dispatching `Executor`
and nothing inside it changes.

## Data

Migration 00003:

```sql
CREATE TABLE tasks (
    name              text        PRIMARY KEY,
    cadence_seconds   integer     NOT NULL CHECK (cadence_seconds > 0),
    enabled_for_roles text[]      NOT NULL DEFAULT '{}',
    enabled           boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now()
);
```

`name` is the primary key rather than a surrogate id: the name is already
the registry key in `internal/tasks`, and a row naming a task the registry
does not know is a configuration error worth failing loudly on at startup —
the same spirit as graph validation refusing unknown screens.

Seeded rows:

| name | cadence | enabled_for_roles |
|---|---|---|
| `help_all` | 30m | `{main, farm, scout, alliance_data}` |
| `daily_gather` | 4h | `{main, farm, scout, alliance_data}` |
| `radar` | 3h | `{main, farm, scout, alliance_data}` |
| `mail_collect` | 24h | `{main, farm, scout, alliance_data}` |
| `tech_donate` | 24h | `{main, farm, scout, alliance_data}` |

Every seed enables every role. `help_all` and `tech_donate` genuinely
require alliance membership, but membership is not what `role` encodes — a
`farm` account may well be in an alliance — so gating them by role would be
wrong, not merely coarse. They are instead expected to no-op safely: their
skeletons treat a missing anchor as "nothing to do" (`ErrAnchorNotFound` →
`nil`), which is exactly what an alliance-less account sees. Real alliance
gating waits for the alliance tables.

Cadences change with a single `UPDATE`, no redeploy.

One `Snapshot` is read per tick, scoped to a caller-supplied set of device
serials (see the loop section): the global pause flag, the accounts on those
devices (id, role, enabled), enabled task rows, and per `(account, task)`
pair the last run time plus consecutive-failure count. The last two come from
`task_runs`, so backoff state is durable rather than in-memory.

## Plan — the whole policy in one pure function

```go
type Decision struct {
    AccountID int64
    TaskName  string
    Overdue   time.Duration // how far past due — the ordering key
}

func Plan(now time.Time, s Snapshot) []Decision
```

A `(account, task)` pair is skipped when any of these hold: the global pause
flag is set, the account is disabled, the task is disabled, the account's
role is not in `enabled_for_roles`, `now` falls inside the account's offline
window, or the cooldown has not elapsed. Survivors become `Decision`s.

Due time is `lastRun + cadence + jitter + backoff(consecutiveFailures)`. A
pair that has never run is due immediately.

Ordering is longest-overdue first, tie-broken by task name ascending.

**Cadence jitter is derived, not drawn.** Firing `help_all` on exact
30-minute boundaries is the fixed pattern invariant #7 exists to defeat, but
`Plan` must stay pure, so the offset comes from
`hash(accountID, taskName, lastRun) → ±20% of cadence`. Identical inputs
always yield the same offset, so re-planning within a tick cannot flip a
decision and a restart cannot reroll one — yet the offset moves across runs,
because `lastRun` moves.

**Backoff** is `cadence × 2^min(failures, 5)`, where `failures` is the count
of consecutive `failed` runs since that pair's last `succeeded` run, read
from `task_runs` rather than held in memory. A task failing every tick because a
game update moved its screen backs off to hours instead of hammering the
device.

## Offline window

```go
func OfflineWindow(accountID int64, date time.Time) (start, end time.Time)
```

Pure. A hash of `(accountID, local date)` picks a start time within the day
and a duration in [5h, 7h]; both move daily because the date is an input.
No storage and no assignment job, and a restart recomputes the identical
window rather than granting a fresh 24h of eligibility.

Wrap-around is the fiddly part: a window starting 22:00 for 6h ends at 04:00
the following day, so testing whether `now` is inside must check both
today's window and yesterday's.

Derivation happens in one configurable `time.Location`, default UTC.
Per-account server timezones are real — an account on a US server should
sleep on US hours — but server timezone is not modelled yet, so a uniformly
placed daily window is the honest approximation. Listed under Deferred
rather than presented as solved.

## The loop

```go
type Store interface {
    SchedulerSnapshot(ctx context.Context, serials []string) (Snapshot, error)
}
type Executor interface {
    Execute(ctx context.Context, accountID int64, taskName string) error
}
```

Each tick: read snapshot → `Plan` → if the plan is empty, sleep a jittered
tick interval (default 60s, jittered ±25%); otherwise execute the **top
decision only**, then re-plan on the next tick. Executing one at a time is both more human (no burst of five
tasks back to back) and more correct: `task_runs` has just changed, so any
plan computed before the execution is stale.

Failure policy, all of which keeps the loop alive:

- **Snapshot read fails** → log, sleep, retry. A Postgres blip must not kill
  a scheduler meant to run for days.
- **Task fails** → log and continue. `runtime.Run` already wrote the failed
  row, which feeds backoff on the next tick, so the loop needs no memory.
- **Paused** → `Plan` returns nothing and the loop idles. `runtime.Run`
  records a pause as status `paused`, not `failed`, so pausing the fleet
  never inflates a task's backoff.
- **Context cancelled** → clean shutdown, including mid-sleep.

Two kill-switch layers, deliberately: the scheduler declines to *start* work
while paused, and `runtime.Ctx` stops work already *in flight*. Neither
subsumes the other.

`agent run` opens the transport per task execution rather than once for the
loop's lifetime — a device that drops off adb fails one task and recovers on
the next tick instead of poisoning a long-lived handle.

Which accounts it drives needs care, because the database has no concept of
"this host". The agent resolves attached serials over adb at startup and
restricts the snapshot to accounts whose device serial is among them;
`--account` narrows further to one. An account registered against a device
that is not attached here is simply not this agent's business, and silently
skipping it is correct — two agents on two hosts must not both drive it.
Sequential execution then means "never two tasks on one device" holds by
construction.
Registry and graph load once at startup and validate loudly, plus one extra
startup check: every enabled row in `tasks` must name a task the registry
knows, or the loop refuses to start.

## Testing

Device-free throughout; DB-free except the query layer.

- **`Plan`** — table-driven: cooldown elapsed and not, never-run, role
  mismatch, task disabled, account disabled, global pause, inside the
  offline window, backoff applied, overdue-first ordering, name tie-break.
- **`OfflineWindow`** — duration always in [5h, 7h]; determinism for
  identical inputs; drift across consecutive days; containment across the
  midnight wrap; start-time spread across many accounts.
- **`backoff`** — growth and cap.
- **Loop** — fake `Store` and `Executor`: top decision runs first, pause
  idles without executing, an executor error does not kill the loop, a
  snapshot error does not kill the loop, cancel shuts down cleanly, never
  two executions in flight.
- **`SchedulerSnapshot`** — integration-tagged: correct last-run times,
  consecutive-failure counts that reset on success and ignore `paused`,
  role filtering, disabled rows excluded.

## Deferred

- Priority, energy cost, and game-state preconditions from the design doc's
  fuller queue model — they need live game state.
- Per-account cadence overrides (M3's per-account schedules).
- Multi-device concurrency and control-side dispatch (M3 fleet).
- REST `POST /fleet/pause` (API milestone).
- Per-account server timezones for the offline window.
- The unattended-24h acceptance run, which needs the phone.
