# Scheduler Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the device-free scheduler slice — a pure `Plan` engine, offline-window and backoff helpers, a thin run loop, a `tasks` config table with its snapshot query, the radar skeleton split, and an `agent run` command — per `docs/superpowers/specs/2026-07-23-scheduler-design.md`.

**Architecture:** `Plan(now, Snapshot) []Decision` is a pure function; all scheduling policy (cadence, backoff, weekday gate, offline window, ordering) lives there and is unit-tested with no database or device. A thin `Loop` does the I/O — read a `Snapshot` from Postgres, call `Plan`, execute the single most-overdue decision, sleep, repeat — with execution behind an `Executor` interface faked in tests. Randomness (cadence jitter, offline window) is *derived by hashing stable inputs*, not drawn from a RNG, so it is pure, restart-stable, and drifts daily.

**Tech Stack:** Go (CGO_ENABLED=0), pgx/goose (existing), `internal/runtime` (existing, wired only in `cmd/agent`), `internal/tasks` registry (existing), `hash/fnv` stdlib.

## Global Constraints

- `CGO_ENABLED=0` always; `make verify-nocgo` must stay green.
- No absolute pixel coordinates outside a `Transport` implementation (not touched here, but the executor wiring must keep using `runtime`, never raw taps).
- Sentinel errors compared with `errors.Is`, never by string.
- All logging via `log/slog` to stderr; CLI results to stdout (pipeable).
- Errors wrapped with `%w` plus locating context (which account, which task).
- No bare `time.Sleep` in loop code — the loop's own sleep is a `select` on `ctx.Done()` and a timer, and is jittered.
- `context.Context` is the first parameter of anything doing I/O.
- Unit tests pass with nothing running; DB-touching tests are `//go:build integration` against `lw_manager_test` via `internal/dbtest`.
- Commit messages: plain, no AI attribution (no `Co-Authored-By`).
- Module path: `github.com/tomharris/lw-manager`.
- Weekday numbers are Go `time.Weekday` (0=Sunday … 6=Saturday). VS days = Mon/Wed/Fri/Sat = `{1,3,5,6}`.

## Branch

```bash
git checkout m1-vision-core && git checkout -b scheduler
```

(`m1-vision-core` carries the merged M2 work and is the base; it is not yet on `main`.)

## File Structure

```
internal/db/migrations/00003_tasks.sql   CREATE  tasks table + seed rows
internal/db/scheduler.go                 CREATE  SchedulerSnapshot query + plain row types
internal/db/scheduler_integration_test.go CREATE integration test for the query
internal/scheduler/window.go             CREATE  hash64, OfflineWindow, inOfflineWindow
internal/scheduler/backoff.go            CREATE  backoff, cadenceOffset
internal/scheduler/schedule.go           CREATE  Snapshot/Account/Task/Decision types, Plan
internal/scheduler/loop.go               CREATE  Store/Executor ifaces, Loop
internal/scheduler/dbstore.go            CREATE  DBStore adapter: db types → scheduler.Snapshot
internal/tasks/radar.go                  DELETE  replaced by the two split files
internal/tasks/radar_quick.go            CREATE  radar_quick skeleton
internal/tasks/radar_claim.go            CREATE  radar_claim skeleton
internal/tasks/tasks_test.go             MODIFY  update expected names + scripts for the split
cmd/agent/main.go                        MODIFY  `agent run` command + runtimeExecutor
CLAUDE.md                                MODIFY  quickstart + layout
```

Dependency direction: `internal/db` returns plain row types and never imports `scheduler`. `internal/scheduler` imports `db` (for the row types its `DBStore` maps) but never imports `runtime` — the executor that wires `runtime` lives in `cmd/agent`. `Plan`, `OfflineWindow`, and `backoff` are pure and import only the stdlib.

Test package placement: `window`, `backoff`, `schedule`, `loop`, and `dbstore` tests are white-box `package scheduler` (they use unexported helpers and package-local fakes). The db query test is `package db` with `//go:build integration`.

---

### Task 1: `tasks` table, seed, and the snapshot query

**Files:**
- Create: `internal/db/migrations/00003_tasks.sql`
- Create: `internal/db/scheduler.go`
- Test: `internal/db/scheduler_integration_test.go`

**Interfaces:**
- Consumes: existing `seedAccount(t, pool)` from `internal/db/runtime_integration_test.go` (creates a device/instance/account on serial `test-runtime-5554`), and `StartTaskRun`/`FinishTaskRun`.
- Produces (all in `package db`):
  - `type ScheduledAccount struct { ID int64; Role string; Enabled bool }`
  - `type ScheduledTask struct { Name string; CadenceSeconds int; Roles []string; DaysOfWeek []int16; Enabled bool }`
  - `type TaskRunState struct { AccountID int64; TaskName string; LastRun *time.Time; ConsecutiveFailures int }`
  - `type SchedulerSnapshot struct { Paused bool; Accounts []ScheduledAccount; Tasks []ScheduledTask; Runs []TaskRunState }`
  - `(*Pool).SchedulerSnapshot(ctx context.Context, serials []string) (SchedulerSnapshot, error)`

- [ ] **Step 1: Write the migration** — `internal/db/migrations/00003_tasks.sql`:

```sql
-- +goose Up

-- Scheduler config: one row per schedulable task. name is the primary key
-- because it is already the registry key in internal/tasks; a row naming a
-- task the registry does not know is a config error the agent fails on at
-- startup. days_of_week holds Go time.Weekday numbers (0=Sun … 6=Sat); the
-- default is every day, a subset gates the task to those weekdays.
CREATE TABLE tasks (
    name              text        PRIMARY KEY,
    cadence_seconds   integer     NOT NULL CHECK (cadence_seconds > 0),
    enabled_for_roles text[]      NOT NULL DEFAULT '{}',
    days_of_week      smallint[]  NOT NULL DEFAULT '{0,1,2,3,4,5,6}',
    enabled           boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now()
);

INSERT INTO tasks (name, cadence_seconds, enabled_for_roles, days_of_week) VALUES
    ('help_all',     180,   '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('daily_gather', 14400, '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('tech_donate',  7200,  '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('mail_collect', 86400, '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('radar_quick',  10800, '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('radar_claim',  86400, '{main,farm,scout,alliance_data}', '{1,3,5,6}');

-- +goose Down
DROP TABLE tasks;
```

- [ ] **Step 2: Write the failing integration test** — `internal/db/scheduler_integration_test.go`:

```go
//go:build integration

package db

import (
	"context"
	"testing"
)

func TestSchedulerSnapshotSeedRows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedAccount(t, pool) // device serial test-runtime-5554

	snap, err := pool.SchedulerSnapshot(ctx, []string{"test-runtime-5554"})
	if err != nil {
		t.Fatalf("SchedulerSnapshot: %v", err)
	}

	byName := map[string]ScheduledTask{}
	for _, tk := range snap.Tasks {
		byName[tk.Name] = tk
	}
	if got := byName["help_all"].CadenceSeconds; got != 180 {
		t.Errorf("help_all cadence: got %d, want 180", got)
	}
	if got := byName["tech_donate"].CadenceSeconds; got != 7200 {
		t.Errorf("tech_donate cadence: got %d, want 7200", got)
	}
	rc, ok := byName["radar_claim"]
	if !ok {
		t.Fatal("radar_claim not in snapshot")
	}
	if len(rc.DaysOfWeek) != 4 {
		t.Fatalf("radar_claim days: got %v, want {1,3,5,6}", rc.DaysOfWeek)
	}
	if _, ok := byName["radar"]; ok {
		t.Error("old single 'radar' task still present; it should have split")
	}
}

func TestSchedulerSnapshotScopesAccountsToSerials(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acctID := seedAccount(t, pool)

	snap, err := pool.SchedulerSnapshot(ctx, []string{"test-runtime-5554"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range snap.Accounts {
		if a.ID == acctID {
			found = true
		}
	}
	if !found {
		t.Fatalf("account %d not in snapshot for its serial", acctID)
	}

	// A serial no device has must yield no accounts.
	empty, err := pool.SchedulerSnapshot(ctx, []string{"no-such-serial"})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Accounts) != 0 {
		t.Fatalf("unknown serial returned %d accounts", len(empty.Accounts))
	}
}

func TestSchedulerSnapshotRunState(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acctID := seedAccount(t, pool)

	// One succeeded run, then two failed: consecutive failures = 2,
	// last_run = the most recent (failed) attempt.
	id1, _ := pool.StartTaskRun(ctx, acctID, "help_all")
	if err := pool.FinishTaskRun(ctx, id1, "succeeded", nil, nil); err != nil {
		t.Fatal(err)
	}
	id2, _ := pool.StartTaskRun(ctx, acctID, "help_all")
	msg := "boom"
	if err := pool.FinishTaskRun(ctx, id2, "failed", &msg, nil); err != nil {
		t.Fatal(err)
	}
	id3, _ := pool.StartTaskRun(ctx, acctID, "help_all")
	if err := pool.FinishTaskRun(ctx, id3, "failed", &msg, nil); err != nil {
		t.Fatal(err)
	}
	// A paused run must neither count as a failure nor reset the streak.
	id4, _ := pool.StartTaskRun(ctx, acctID, "help_all")
	if err := pool.FinishTaskRun(ctx, id4, "paused", nil, nil); err != nil {
		t.Fatal(err)
	}

	snap, err := pool.SchedulerSnapshot(ctx, []string{"test-runtime-5554"})
	if err != nil {
		t.Fatal(err)
	}
	var rs *TaskRunState
	for i := range snap.Runs {
		if snap.Runs[i].AccountID == acctID && snap.Runs[i].TaskName == "help_all" {
			rs = &snap.Runs[i]
		}
	}
	if rs == nil {
		t.Fatal("no run state for (account, help_all)")
	}
	if rs.ConsecutiveFailures != 2 {
		t.Errorf("consecutive failures: got %d, want 2 (paused ignored)", rs.ConsecutiveFailures)
	}
	if rs.LastRun == nil {
		t.Error("last_run is nil; a failed run should still count as having run")
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `docker compose up -d && go test -tags=integration ./internal/db/ -run TestSchedulerSnapshot -v`
Expected: FAIL — `SchedulerSnapshot` undefined (compile error).

- [ ] **Step 4: Implement** — `internal/db/scheduler.go`:

```go
package db

import (
	"context"
	"fmt"
	"time"
)

// ScheduledAccount is one account the scheduler may drive.
type ScheduledAccount struct {
	ID      int64
	Role    string
	Enabled bool
}

// ScheduledTask is one row of the tasks config table.
type ScheduledTask struct {
	Name           string
	CadenceSeconds int
	Roles          []string
	DaysOfWeek     []int16 // Go time.Weekday numbers
	Enabled        bool
}

// TaskRunState summarises task_runs for one (account, task) pair.
type TaskRunState struct {
	AccountID           int64
	TaskName            string
	LastRun             *time.Time
	ConsecutiveFailures int
}

// SchedulerSnapshot is everything the scheduler needs for one planning tick.
type SchedulerSnapshot struct {
	Paused   bool
	Accounts []ScheduledAccount
	Tasks    []ScheduledTask
	Runs     []TaskRunState
}

// SchedulerSnapshot reads the pause flag, the accounts on the given device
// serials, the task config, and each (account, task) pair's run state.
// Scoping by serials is how one agent drives only the devices attached to
// its own host: an account on a device not attached here is not its business.
func (p *Pool) SchedulerSnapshot(ctx context.Context, serials []string) (SchedulerSnapshot, error) {
	var snap SchedulerSnapshot

	if err := p.QueryRow(ctx, `SELECT pause_all FROM flags`).Scan(&snap.Paused); err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: reading pause flag: %w", err)
	}

	rows, err := p.Query(ctx, `
		SELECT a.id, a.role, a.enabled
		FROM accounts a
		JOIN app_instances ai ON ai.id = a.app_instance_id
		JOIN devices d        ON d.id  = ai.device_id
		WHERE d.serial = ANY($1)`, serials)
	if err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: reading scheduled accounts: %w", err)
	}
	for rows.Next() {
		var a ScheduledAccount
		if err := rows.Scan(&a.ID, &a.Role, &a.Enabled); err != nil {
			rows.Close()
			return SchedulerSnapshot{}, fmt.Errorf("db: scanning scheduled account: %w", err)
		}
		snap.Accounts = append(snap.Accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: iterating scheduled accounts: %w", err)
	}

	trows, err := p.Query(ctx, `
		SELECT name, cadence_seconds, enabled_for_roles, days_of_week, enabled
		FROM tasks`)
	if err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: reading tasks: %w", err)
	}
	for trows.Next() {
		var t ScheduledTask
		if err := trows.Scan(&t.Name, &t.CadenceSeconds, &t.Roles, &t.DaysOfWeek, &t.Enabled); err != nil {
			trows.Close()
			return SchedulerSnapshot{}, fmt.Errorf("db: scanning task: %w", err)
		}
		snap.Tasks = append(snap.Tasks, t)
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: iterating tasks: %w", err)
	}

	// Per (account, task): last_run is the most recent attempt that actually
	// ran (succeeded or failed — paused/running did no work and must not
	// push the cadence out). consec_failures counts failed runs since the
	// last success, so a pause neither counts nor resets the streak.
	rrows, err := p.Query(ctx, `
		WITH scoped AS (
			SELECT a.id AS account_id
			FROM accounts a
			JOIN app_instances ai ON ai.id = a.app_instance_id
			JOIN devices d        ON d.id  = ai.device_id
			WHERE d.serial = ANY($1)
		),
		agg AS (
			SELECT tr.account_id, tr.task_name,
			       max(tr.started_at) FILTER (WHERE tr.status IN ('succeeded','failed')) AS last_run,
			       max(tr.started_at) FILTER (WHERE tr.status = 'succeeded')             AS last_success
			FROM task_runs tr
			JOIN scoped s ON s.account_id = tr.account_id
			GROUP BY tr.account_id, tr.task_name
		)
		SELECT agg.account_id, agg.task_name, agg.last_run,
		       (SELECT count(*) FROM task_runs f
		        WHERE f.account_id = agg.account_id
		          AND f.task_name  = agg.task_name
		          AND f.status = 'failed'
		          AND f.started_at > COALESCE(agg.last_success, '-infinity')) AS consec_failures
		FROM agg`, serials)
	if err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: reading run state: %w", err)
	}
	for rrows.Next() {
		var rs TaskRunState
		if err := rrows.Scan(&rs.AccountID, &rs.TaskName, &rs.LastRun, &rs.ConsecutiveFailures); err != nil {
			rrows.Close()
			return SchedulerSnapshot{}, fmt.Errorf("db: scanning run state: %w", err)
		}
		snap.Runs = append(snap.Runs, rs)
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: iterating run state: %w", err)
	}

	return snap, nil
}
```

- [ ] **Step 5: Run tests**

Run: `go test -tags=integration ./internal/db/ -run TestSchedulerSnapshot -v && go test ./...`
Expected: PASS all three integration tests; unit tier unaffected.

- [ ] **Step 6: Commit**

```bash
git add internal/db/
git commit -m "Add tasks config table and scheduler snapshot query"
```

---

### Task 2: Offline window

**Files:**
- Create: `internal/scheduler/window.go`
- Test: `internal/scheduler/window_test.go` (`package scheduler`)

**Interfaces:**
- Produces (unexported except where noted):
  - `func hash64(parts ...string) uint64`
  - `func OfflineWindow(accountID int64, date time.Time) (start, end time.Time)` (exported)
  - `func inOfflineWindow(accountID int64, now time.Time) bool`

- [ ] **Step 1: Write failing tests** — `internal/scheduler/window_test.go`:

```go
package scheduler

import (
	"testing"
	"time"
)

func TestOfflineWindowDurationInRange(t *testing.T) {
	loc := time.UTC
	for id := int64(1); id <= 500; id++ {
		date := time.Date(2026, 7, 24, 12, 0, 0, 0, loc)
		start, end := OfflineWindow(id, date)
		dur := end.Sub(start)
		if dur < 5*time.Hour || dur > 7*time.Hour {
			t.Fatalf("account %d: duration %v outside [5h,7h]", id, dur)
		}
		if start.Before(time.Date(2026, 7, 24, 0, 0, 0, 0, loc)) ||
			!start.Before(time.Date(2026, 7, 25, 0, 0, 0, 0, loc)) {
			t.Fatalf("account %d: start %v not within its day", id, start)
		}
	}
}

func TestOfflineWindowDeterministic(t *testing.T) {
	date := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	s1, e1 := OfflineWindow(42, date)
	s2, e2 := OfflineWindow(42, date)
	if !s1.Equal(s2) || !e1.Equal(e2) {
		t.Fatal("same inputs produced different windows")
	}
}

func TestOfflineWindowDriftsDaily(t *testing.T) {
	d1 := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	s1, _ := OfflineWindow(42, d1)
	s2, _ := OfflineWindow(42, d2)
	// Compare start-of-day offsets, not absolute times.
	off1 := s1.Sub(time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC))
	off2 := s2.Sub(time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC))
	if off1 == off2 {
		t.Fatal("window start did not drift between consecutive days")
	}
}

func TestInOfflineWindowContainsAndExcludes(t *testing.T) {
	id := int64(7)
	date := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	start, end := OfflineWindow(id, date)

	mid := start.Add(end.Sub(start) / 2)
	if !inOfflineWindow(id, mid) {
		t.Fatalf("midpoint %v reported outside window [%v,%v]", mid, start, end)
	}
	if inOfflineWindow(id, start.Add(-time.Minute)) {
		t.Fatal("instant just before start reported inside")
	}
	if inOfflineWindow(id, end.Add(time.Minute)) {
		t.Fatal("instant just after end reported inside")
	}
}

func TestInOfflineWindowHandlesMidnightWrap(t *testing.T) {
	// Find an account whose window wraps past midnight, then assert a time
	// in the early-morning tail (owned by yesterday's window) is detected.
	loc := time.UTC
	day := time.Date(2026, 7, 24, 0, 0, 0, 0, loc)
	for id := int64(1); id <= 2000; id++ {
		start, end := OfflineWindow(id, day)
		if end.Day() == start.Day() {
			continue // does not wrap
		}
		tail := time.Date(2026, 7, 25, 0, 30, 0, 0, loc) // 00:30 next day
		if !tail.Before(end) {
			continue
		}
		if !inOfflineWindow(id, tail) {
			t.Fatalf("account %d: %v inside wrapped window [%v,%v] not detected", id, tail, start, end)
		}
		return
	}
	t.Skip("no wrapping window found in 2000 accounts (unlikely); widen search if this trips")
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/scheduler/ -v`
Expected: FAIL — package does not exist / identifiers undefined.

- [ ] **Step 3: Implement** — `internal/scheduler/window.go`:

```go
// Package scheduler decides which task to run for which account, and when.
//
// The policy lives in Plan, a pure function over a Snapshot. All randomness
// (cadence jitter, offline window) is derived by hashing stable inputs
// rather than drawn from a RNG: that keeps Plan pure and testable, keeps a
// process restart from re-rolling a decision, and still drifts across days
// because the date is one of the hashed inputs.
package scheduler

import (
	"hash/fnv"
	"strconv"
	"time"
)

// hash64 is a stable FNV-1a hash of its parts, NUL-separated so ("a","bc")
// and ("ab","c") differ.
func hash64(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// OfflineWindow returns the account's do-not-run window for the given date's
// day, computed in that date's location. The platform must not run a device
// 24/7 (detection avoidance): each account gets a 5–7h daily offline window
// whose start and length drift daily. Derived, not stored, so a restart
// recomputes the identical window instead of granting fresh eligibility.
func OfflineWindow(accountID int64, date time.Time) (start, end time.Time) {
	y, m, d := date.Date()
	loc := date.Location()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, loc)

	h := hash64(strconv.FormatInt(accountID, 10), date.Format("2006-01-02"))
	startMin := int(h % (24 * 60))        // any minute of the day
	durMin := 300 + int((h/1440)%121)     // 300..420 minutes = 5h..7h

	start = midnight.Add(time.Duration(startMin) * time.Minute)
	end = start.Add(time.Duration(durMin) * time.Minute)
	return start, end
}

// inOfflineWindow reports whether now falls in the account's offline window.
// It checks today's window and yesterday's, because a window that starts
// late (e.g. 22:00) runs past midnight into the following morning.
func inOfflineWindow(accountID int64, now time.Time) bool {
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		start, end := OfflineWindow(accountID, day)
		if !now.Before(start) && now.Before(end) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/scheduler/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/window.go internal/scheduler/window_test.go
git commit -m "Add derived offline-window helper"
```

---

### Task 3: Backoff and cadence jitter

**Files:**
- Create: `internal/scheduler/backoff.go`
- Test: `internal/scheduler/backoff_test.go` (`package scheduler`)

**Interfaces:**
- Consumes: `hash64` from Task 2.
- Produces:
  - `func backoff(cadence time.Duration, failures int) time.Duration` — the *extra* delay added on top of the base cadence, so total spacing after `failures` is `cadence × 2^min(failures,5)`.
  - `func cadenceOffset(accountID int64, taskName string, lastRun time.Time, cadence time.Duration) time.Duration` — derived jitter in `[-0.2, +0.2] × cadence`.

- [ ] **Step 1: Write failing tests** — `internal/scheduler/backoff_test.go`:

```go
package scheduler

import (
	"testing"
	"time"
)

func TestBackoffGrowsAndCaps(t *testing.T) {
	c := time.Hour
	cases := map[int]time.Duration{
		0: 0,
		1: c,       // total spacing 2c
		2: 3 * c,   // total spacing 4c
		3: 7 * c,   // total spacing 8c
		5: 31 * c,  // total spacing 32c
		9: 31 * c,  // capped at failures=5
	}
	for failures, want := range cases {
		if got := backoff(c, failures); got != want {
			t.Errorf("backoff(1h, %d): got %v, want %v", failures, got, want)
		}
	}
}

func TestCadenceOffsetWithinBounds(t *testing.T) {
	c := time.Hour
	limit := time.Duration(0.2 * float64(c))
	seen := map[time.Duration]bool{}
	for i := 0; i < 300; i++ {
		last := time.Unix(int64(i*97), 0)
		off := cadenceOffset(int64(i), "help_all", last, c)
		if off < -limit || off > limit {
			t.Fatalf("offset %v outside ±20%% of %v", off, c)
		}
		seen[off] = true
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct offsets in 300 draws — not jittered", len(seen))
	}
}

func TestCadenceOffsetDeterministic(t *testing.T) {
	c := time.Hour
	last := time.Unix(1000, 0)
	if cadenceOffset(5, "radar_quick", last, c) != cadenceOffset(5, "radar_quick", last, c) {
		t.Fatal("same inputs produced different offsets")
	}
	// A different lastRun moves the offset (drift across runs).
	if cadenceOffset(5, "radar_quick", last, c) == cadenceOffset(5, "radar_quick", last.Add(time.Second), c) {
		t.Fatal("offset did not move when lastRun changed")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/scheduler/ -run 'TestBackoff|TestCadence' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement** — `internal/scheduler/backoff.go`:

```go
package scheduler

import (
	"strconv"
	"time"
)

// maxBackoffFailures caps the exponential backoff so a permanently-broken
// task settles at a long-but-bounded retry interval instead of overflowing.
const maxBackoffFailures = 5

// backoff is the extra delay added to a task's base cadence after a run of
// consecutive failures, so the total spacing from the last attempt is
// cadence × 2^min(failures, maxBackoffFailures). Zero when nothing has
// failed. Failures beyond the cap reuse the cap's value.
func backoff(cadence time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures > maxBackoffFailures {
		failures = maxBackoffFailures
	}
	return cadence * time.Duration((int64(1)<<uint(failures))-1)
}

// cadenceOffset is a derived jitter in [-0.2, +0.2] × cadence. Firing a task
// on exact cadence boundaries is the fixed pattern invariant #7 exists to
// defeat, but Plan must stay pure — so the offset is hashed from stable
// inputs. Including lastRun makes it move run-to-run while staying fixed
// within a single planning tick.
func cadenceOffset(accountID int64, taskName string, lastRun time.Time, cadence time.Duration) time.Duration {
	h := hash64(strconv.FormatInt(accountID, 10), taskName, strconv.FormatInt(lastRun.UnixNano(), 10))
	frac := (float64(h%40001)/40000.0)*0.4 - 0.2 // -0.2 … +0.2
	return time.Duration(frac * float64(cadence))
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/scheduler/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/backoff.go internal/scheduler/backoff_test.go
git commit -m "Add backoff and derived cadence jitter"
```

---

### Task 4: `Plan` and the scheduler types

**Files:**
- Create: `internal/scheduler/schedule.go`
- Test: `internal/scheduler/schedule_test.go` (`package scheduler`)

**Interfaces:**
- Consumes: `inOfflineWindow` (Task 2), `backoff` + `cadenceOffset` (Task 3).
- Produces:
  - `type Account struct { ID int64; Role string; Enabled bool }`
  - `type Task struct { Name string; Cadence time.Duration; Roles []string; Days []time.Weekday; Enabled bool }`
  - `type RunKey struct { AccountID int64; TaskName string }`
  - `type RunState struct { LastRun time.Time; HasRun bool; ConsecutiveFailures int }`
  - `type Snapshot struct { Paused bool; Accounts []Account; Tasks []Task; Runs map[RunKey]RunState }`
  - `type Decision struct { AccountID int64; TaskName string; Overdue time.Duration }`
  - `func Plan(now time.Time, s Snapshot) []Decision` — `now` is expected already in the operator's location (the loop's responsibility); `Plan` uses `now.Weekday()` and `OfflineWindow` via `now`.

- [ ] **Step 1: Write failing tests** — `internal/scheduler/schedule_test.go`:

```go
package scheduler

import (
	"testing"
	"time"
)

// A Wednesday noon well outside any tested account's offline window is hard
// to guarantee, so tests that must avoid the window pick their account id
// after checking inOfflineWindow, or use a task/time the window does not
// cover. Here we choose a fixed instant and, where needed, assert around it.
var wed = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) // Wednesday (weekday 3)

func allDays() []time.Weekday {
	return []time.Weekday{0, 1, 2, 3, 4, 5, 6}
}

// baseSnapshot: one enabled account (farm role) and one enabled all-day
// task, never run. now is a time not inside the account's offline window.
func baseSnapshot(t *testing.T) (Snapshot, time.Time) {
	t.Helper()
	acctID := int64(1)
	// Advance the instant off the account's window if it happens to overlap.
	now := wed
	for inOfflineWindow(acctID, now) {
		now = now.Add(time.Hour)
	}
	return Snapshot{
		Accounts: []Account{{ID: acctID, Role: "farm", Enabled: true}},
		Tasks:    []Task{{Name: "help_all", Cadence: 3 * time.Minute, Roles: []string{"farm"}, Days: allDays(), Enabled: true}},
		Runs:     map[RunKey]RunState{},
	}, now
}

func TestPlanNeverRunIsDueImmediately(t *testing.T) {
	s, now := baseSnapshot(t)
	got := Plan(now, s)
	if len(got) != 1 || got[0].TaskName != "help_all" || got[0].AccountID != 1 {
		t.Fatalf("plan: %+v", got)
	}
}

func TestPlanCooldownNotElapsed(t *testing.T) {
	s, now := baseSnapshot(t)
	s.Runs[RunKey{1, "help_all"}] = RunState{LastRun: now.Add(-1 * time.Minute), HasRun: true}
	if got := Plan(now, s); len(got) != 0 {
		t.Fatalf("task within cooldown planned: %+v", got)
	}
}

func TestPlanCooldownElapsed(t *testing.T) {
	s, now := baseSnapshot(t)
	// 3m cadence ±20% ≤ 3.6m; 10m ago is safely due.
	s.Runs[RunKey{1, "help_all"}] = RunState{LastRun: now.Add(-10 * time.Minute), HasRun: true}
	if got := Plan(now, s); len(got) != 1 {
		t.Fatalf("elapsed task not planned: %+v", got)
	}
}

func TestPlanSkipsWhenPaused(t *testing.T) {
	s, now := baseSnapshot(t)
	s.Paused = true
	if got := Plan(now, s); got != nil {
		t.Fatalf("planned while paused: %+v", got)
	}
}

func TestPlanSkipsDisabledAccountAndTask(t *testing.T) {
	s, now := baseSnapshot(t)
	s.Accounts[0].Enabled = false
	if got := Plan(now, s); len(got) != 0 {
		t.Fatalf("planned for disabled account: %+v", got)
	}
	s.Accounts[0].Enabled = true
	s.Tasks[0].Enabled = false
	if got := Plan(now, s); len(got) != 0 {
		t.Fatalf("planned a disabled task: %+v", got)
	}
}

func TestPlanSkipsRoleMismatch(t *testing.T) {
	s, now := baseSnapshot(t)
	s.Tasks[0].Roles = []string{"alliance_data"} // account is farm
	if got := Plan(now, s); len(got) != 0 {
		t.Fatalf("planned despite role mismatch: %+v", got)
	}
}

func TestPlanWeekdayGate(t *testing.T) {
	s, now := baseSnapshot(t) // now is a Wednesday (weekday 3)
	s.Tasks[0].Days = []time.Weekday{time.Monday, time.Friday} // not Wednesday
	if got := Plan(now, s); len(got) != 0 {
		t.Fatalf("planned on a disallowed weekday: %+v", got)
	}
	s.Tasks[0].Days = []time.Weekday{time.Wednesday}
	if got := Plan(now, s); len(got) != 1 {
		t.Fatalf("not planned on an allowed weekday: %+v", got)
	}
}

func TestPlanSkipsInsideOfflineWindow(t *testing.T) {
	acctID := int64(1)
	// Pick an instant known to be inside the account's window.
	day := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	start, end := OfflineWindow(acctID, day)
	mid := start.Add(end.Sub(start) / 2)
	s := Snapshot{
		Accounts: []Account{{ID: acctID, Role: "farm", Enabled: true}},
		Tasks:    []Task{{Name: "help_all", Cadence: 3 * time.Minute, Roles: []string{"farm"}, Days: allDays(), Enabled: true}},
		Runs:     map[RunKey]RunState{},
	}
	if got := Plan(mid, s); len(got) != 0 {
		t.Fatalf("planned inside the offline window: %+v", got)
	}
}

func TestPlanBackoffDelaysRetry(t *testing.T) {
	s, now := baseSnapshot(t)
	// 2 consecutive failures → total spacing 4× cadence = 12m. A run 8m ago
	// is within backoff and must not be planned.
	s.Runs[RunKey{1, "help_all"}] = RunState{LastRun: now.Add(-8 * time.Minute), HasRun: true, ConsecutiveFailures: 2}
	if got := Plan(now, s); len(got) != 0 {
		t.Fatalf("planned during backoff: %+v", got)
	}
	// 20m ago clears 12m even with max negative jitter.
	s.Runs[RunKey{1, "help_all"}] = RunState{LastRun: now.Add(-20 * time.Minute), HasRun: true, ConsecutiveFailures: 2}
	if got := Plan(now, s); len(got) != 1 {
		t.Fatalf("not planned after backoff elapsed: %+v", got)
	}
}

func TestPlanOrdersByOverdueThenName(t *testing.T) {
	acctID := int64(1)
	now := wed
	for inOfflineWindow(acctID, now) {
		now = now.Add(time.Hour)
	}
	s := Snapshot{
		Accounts: []Account{{ID: acctID, Role: "farm", Enabled: true}},
		Tasks: []Task{
			{Name: "help_all", Cadence: time.Hour, Roles: []string{"farm"}, Days: allDays(), Enabled: true},
			{Name: "daily_gather", Cadence: time.Hour, Roles: []string{"farm"}, Days: allDays(), Enabled: true},
			{Name: "mail_collect", Cadence: time.Hour, Roles: []string{"farm"}, Days: allDays(), Enabled: true},
		},
		Runs: map[RunKey]RunState{
			{acctID, "help_all"}:     {LastRun: now.Add(-2 * time.Hour), HasRun: true}, // 1h overdue
			{acctID, "daily_gather"}: {LastRun: now.Add(-5 * time.Hour), HasRun: true}, // 4h overdue → first
			// mail_collect never run → due now, overdue ~0 → last
		},
	}
	got := Plan(now, s)
	if len(got) != 3 {
		t.Fatalf("expected 3 decisions, got %+v", got)
	}
	if got[0].TaskName != "daily_gather" {
		t.Errorf("most overdue first: got %q", got[0].TaskName)
	}
	if got[2].TaskName != "mail_collect" {
		t.Errorf("least overdue (never-run) last: got %q", got[2].TaskName)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/scheduler/ -run TestPlan -v`
Expected: FAIL — `Plan`, `Snapshot`, etc. undefined.

- [ ] **Step 3: Implement** — `internal/scheduler/schedule.go`:

```go
package scheduler

import (
	"sort"
	"time"
)

// Account is one account the scheduler may drive. Only enabled accounts
// should reach Plan, but Plan re-checks Enabled so the policy is authoritative.
type Account struct {
	ID      int64
	Role    string
	Enabled bool
}

// Task is one schedulable task's config.
type Task struct {
	Name    string
	Cadence time.Duration
	Roles   []string
	Days    []time.Weekday // weekdays the task may run
	Enabled bool
}

// RunKey identifies one (account, task) pair.
type RunKey struct {
	AccountID int64
	TaskName  string
}

// RunState is a pair's recent history from task_runs.
type RunState struct {
	LastRun             time.Time
	HasRun              bool
	ConsecutiveFailures int
}

// Snapshot is one planning tick's view of the world.
type Snapshot struct {
	Paused   bool
	Accounts []Account
	Tasks    []Task
	Runs     map[RunKey]RunState
}

// Decision is one (account, task) the scheduler wants to run now, with how
// far past due it is — the ordering key.
type Decision struct {
	AccountID int64
	TaskName  string
	Overdue   time.Duration
}

// Plan returns the tasks due to run at now, most-overdue first. now is
// expected already in the operator's location (the caller's responsibility),
// so now.Weekday() and the offline-window date are the operator's local day.
func Plan(now time.Time, s Snapshot) []Decision {
	if s.Paused {
		return nil
	}
	var out []Decision
	for _, a := range s.Accounts {
		if !a.Enabled {
			continue
		}
		if inOfflineWindow(a.ID, now) {
			continue
		}
		for _, t := range s.Tasks {
			if !t.Enabled || !roleAllowed(a.Role, t.Roles) || !weekdayAllowed(now.Weekday(), t.Days) {
				continue
			}
			rs := s.Runs[RunKey{AccountID: a.ID, TaskName: t.Name}]
			due := dueTime(a.ID, t.Name, t.Cadence, rs, now)
			if now.Before(due) {
				continue
			}
			out = append(out, Decision{AccountID: a.ID, TaskName: t.Name, Overdue: now.Sub(due)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Overdue != out[j].Overdue {
			return out[i].Overdue > out[j].Overdue
		}
		if out[i].TaskName != out[j].TaskName {
			return out[i].TaskName < out[j].TaskName
		}
		return out[i].AccountID < out[j].AccountID
	})
	return out
}

// dueTime is when a pair next becomes eligible. A never-run pair is due now.
func dueTime(accountID int64, taskName string, cadence time.Duration, rs RunState, now time.Time) time.Time {
	if !rs.HasRun {
		return now
	}
	return rs.LastRun.Add(cadence + cadenceOffset(accountID, taskName, rs.LastRun, cadence) + backoff(cadence, rs.ConsecutiveFailures))
}

func roleAllowed(role string, allowed []string) bool {
	for _, r := range allowed {
		if r == role {
			return true
		}
	}
	return false
}

func weekdayAllowed(day time.Weekday, allowed []time.Weekday) bool {
	for _, d := range allowed {
		if d == day {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/scheduler/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/schedule.go internal/scheduler/schedule_test.go
git commit -m "Add pure Plan scheduling policy"
```

---

### Task 5: The loop

**Files:**
- Create: `internal/scheduler/loop.go`
- Test: `internal/scheduler/loop_test.go` (`package scheduler`)

**Interfaces:**
- Consumes: `Plan`, `Snapshot`, `Decision` (Task 4).
- Produces:
  - `type Store interface { SchedulerSnapshot(ctx context.Context, serials []string) (Snapshot, error) }`
  - `type Executor interface { Execute(ctx context.Context, accountID int64, taskName string) error }`
  - `type Options struct { Store Store; Executor Executor; Serials []string; Tick time.Duration; Rand *rand.Rand; Clock func() time.Time; Location *time.Location; Log *slog.Logger }`
  - `func New(opts Options) (*Loop, error)`
  - `func (l *Loop) Run(ctx context.Context) error` — loops until ctx cancelled, then returns nil
  - `func (l *Loop) tickOnce(ctx context.Context) (executed bool, err error)` — unexported, used by tests

- [ ] **Step 1: Write failing tests** — `internal/scheduler/loop_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore returns programmed snapshots; the i-th SchedulerSnapshot call
// returns snaps[min(i, len-1)], holding the last. err (if set) is returned
// on every call.
type fakeStore struct {
	snaps []Snapshot
	err   error
	calls int32
}

func (f *fakeStore) SchedulerSnapshot(ctx context.Context, serials []string) (Snapshot, error) {
	n := int(atomic.AddInt32(&f.calls, 1)) - 1
	if f.err != nil {
		return Snapshot{}, f.err
	}
	if n >= len(f.snaps) {
		n = len(f.snaps) - 1
	}
	return f.snaps[n], nil
}

// recordExec records executions and enforces that none overlap.
type recordExec struct {
	mu       sync.Mutex
	calls    []Decision
	inFlight int32
	maxSeen  int32
	err      error
	after    int // cancel ctx after this many calls (0 = never)
	cancel   context.CancelFunc
}

func (e *recordExec) Execute(ctx context.Context, accountID int64, taskName string) error {
	n := atomic.AddInt32(&e.inFlight, 1)
	if n > atomic.LoadInt32(&e.maxSeen) {
		atomic.StoreInt32(&e.maxSeen, n)
	}
	defer atomic.AddInt32(&e.inFlight, -1)

	e.mu.Lock()
	e.calls = append(e.calls, Decision{AccountID: accountID, TaskName: taskName})
	total := len(e.calls)
	e.mu.Unlock()

	if e.after > 0 && total >= e.after && e.cancel != nil {
		e.cancel()
	}
	return e.err
}

func dueSnapshot() Snapshot {
	return Snapshot{
		Accounts: []Account{{ID: 1, Role: "farm", Enabled: true}},
		Tasks: []Task{
			{Name: "help_all", Cadence: time.Hour, Roles: []string{"farm"}, Days: []time.Weekday{0, 1, 2, 3, 4, 5, 6}, Enabled: true},
			{Name: "daily_gather", Cadence: time.Hour, Roles: []string{"farm"}, Days: []time.Weekday{0, 1, 2, 3, 4, 5, 6}, Enabled: true},
		},
		Runs: map[RunKey]RunState{
			{1, "help_all"}:     {LastRun: time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC), HasRun: true},  // 6h overdue → first
			{1, "daily_gather"}: {LastRun: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC), HasRun: true}, // 2h overdue
		},
	}
}

// noWindowClock returns an instant guaranteed outside account 1's offline
// window, so window timing never flakes these loop tests.
func noWindowClock() func() time.Time {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	for inOfflineWindow(1, now) {
		now = now.Add(time.Hour)
	}
	return func() time.Time { return now }
}

func newTestLoop(t *testing.T, st Store, ex Executor) *Loop {
	t.Helper()
	l, err := New(Options{
		Store:    st,
		Executor: ex,
		Serials:  []string{"s"},
		Tick:     time.Millisecond,
		Clock:    noWindowClock(),
		Location: time.UTC,
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestTickOnceRunsMostOverdueFirst(t *testing.T) {
	ex := &recordExec{}
	l := newTestLoop(t, &fakeStore{snaps: []Snapshot{dueSnapshot()}}, ex)
	executed, err := l.tickOnce(context.Background())
	if err != nil || !executed {
		t.Fatalf("tickOnce: executed=%v err=%v", executed, err)
	}
	if len(ex.calls) != 1 || ex.calls[0].TaskName != "help_all" {
		t.Fatalf("expected one exec of help_all, got %+v", ex.calls)
	}
}

func TestTickOnceIdlesWhenPaused(t *testing.T) {
	s := dueSnapshot()
	s.Paused = true
	ex := &recordExec{}
	l := newTestLoop(t, &fakeStore{snaps: []Snapshot{s}}, ex)
	executed, err := l.tickOnce(context.Background())
	if executed || err != nil {
		t.Fatalf("paused tick executed=%v err=%v", executed, err)
	}
	if len(ex.calls) != 0 {
		t.Fatalf("executed while paused: %+v", ex.calls)
	}
}

func TestRunSurvivesExecutorError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ex := &recordExec{err: errors.New("task boom"), after: 2, cancel: cancel}
	l := newTestLoop(t, &fakeStore{snaps: []Snapshot{dueSnapshot()}}, ex)
	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run returned %v; an executor error must not kill the loop", err)
	}
	if len(ex.calls) < 2 {
		t.Fatalf("loop stopped after first executor error: %d calls", len(ex.calls))
	}
}

func TestRunSurvivesSnapshotError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := &fakeStore{err: errors.New("db down")}
	ex := &recordExec{}
	l := newTestLoop(t, st, ex)
	// Cancel from a goroutine after the store has been polled a few times.
	go func() {
		for atomic.LoadInt32(&st.calls) < 3 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run returned %v; a snapshot error must not kill the loop", err)
	}
	if atomic.LoadInt32(&st.calls) < 3 {
		t.Fatalf("loop stopped after snapshot error: %d calls", st.calls)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := newTestLoop(t, &fakeStore{snaps: []Snapshot{{}}}, &recordExec{})
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunNeverOverlapsExecutions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ex := &recordExec{after: 5, cancel: cancel}
	l := newTestLoop(t, &fakeStore{snaps: []Snapshot{dueSnapshot()}}, ex)
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if ex.maxSeen > 1 {
		t.Fatalf("executions overlapped: max in-flight %d", ex.maxSeen)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/scheduler/ -run 'TestTick|TestRun' -v`
Expected: FAIL — `New`, `Loop` undefined.

- [ ] **Step 3: Implement** — `internal/scheduler/loop.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"
)

// Store reads a planning snapshot for the given device serials.
type Store interface {
	SchedulerSnapshot(ctx context.Context, serials []string) (Snapshot, error)
}

// Executor runs one task for one account. Production wires this to
// runtime.Run over a device transport; tests fake it.
type Executor interface {
	Execute(ctx context.Context, accountID int64, taskName string) error
}

// Options configures a Loop. Store and Executor are required.
type Options struct {
	Store    Store
	Executor Executor
	Serials  []string
	// Tick is the base sleep between planning ticks, jittered ±25%.
	// Default 60s.
	Tick     time.Duration
	Rand     *rand.Rand
	Clock    func() time.Time // default time.Now
	Location *time.Location   // default time.UTC
	Log      *slog.Logger
}

// Loop drives the scheduler: snapshot → Plan → execute the single most
// overdue decision → sleep → repeat. Executing one task per tick keeps the
// device from firing bursts and keeps each plan fresh, since task_runs
// changes after every execution.
type Loop struct {
	store   Store
	exec    Executor
	serials []string
	tick    time.Duration
	rand    *rand.Rand
	clock   func() time.Time
	loc     *time.Location
	log     *slog.Logger
}

// New builds a Loop, applying defaults.
func New(opts Options) (*Loop, error) {
	if opts.Store == nil || opts.Executor == nil {
		return nil, errors.New("scheduler: Store and Executor are required")
	}
	l := &Loop{
		store:   opts.Store,
		exec:    opts.Executor,
		serials: opts.Serials,
		tick:    opts.Tick,
		rand:    opts.Rand,
		clock:   opts.Clock,
		loc:     opts.Location,
		log:     opts.Log,
	}
	if l.tick <= 0 {
		l.tick = 60 * time.Second
	}
	if l.rand == nil {
		l.rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if l.clock == nil {
		l.clock = time.Now
	}
	if l.loc == nil {
		l.loc = time.UTC
	}
	if l.log == nil {
		l.log = slog.Default().With("component", "scheduler")
	}
	return l, nil
}

// Run loops until ctx is cancelled, then returns nil. Transient failures
// (snapshot read, task execution) are logged and swallowed: a scheduler
// meant to run for days must not die on a Postgres blip or one bad task.
func (l *Loop) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := l.tickOnce(ctx); err != nil {
			l.log.Warn("scheduler tick error", "error", err)
		}
		if err := l.sleep(ctx, l.tickInterval()); err != nil {
			return nil // cancelled during sleep
		}
	}
}

// tickOnce reads a snapshot, plans, and executes the single most overdue
// decision. executed reports whether a task ran; err surfaces a snapshot or
// execution failure (Run logs and continues).
func (l *Loop) tickOnce(ctx context.Context) (executed bool, err error) {
	snap, err := l.store.SchedulerSnapshot(ctx, l.serials)
	if err != nil {
		return false, err
	}
	now := l.clock().In(l.loc)
	plan := Plan(now, snap)
	if len(plan) == 0 {
		return false, nil
	}
	d := plan[0]
	if err := l.exec.Execute(ctx, d.AccountID, d.TaskName); err != nil {
		return true, err
	}
	return true, nil
}

func (l *Loop) tickInterval() time.Duration {
	lo := time.Duration(float64(l.tick) * 0.75)
	hi := time.Duration(float64(l.tick) * 1.25)
	if hi <= lo {
		return l.tick
	}
	return lo + time.Duration(l.rand.Int63n(int64(hi-lo)+1))
}

func (l *Loop) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/scheduler/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/loop.go internal/scheduler/loop_test.go
git commit -m "Add scheduler loop with fake-testable execution"
```

---

### Task 6: `DBStore` adapter

**Files:**
- Create: `internal/scheduler/dbstore.go`
- Test: `internal/scheduler/dbstore_test.go` (`package scheduler`)

**Interfaces:**
- Consumes: `db.SchedulerSnapshot` and its row types (Task 1); `Snapshot`/`Account`/`Task`/`RunKey`/`RunState` (Task 4); `Store` (Task 5).
- Produces:
  - `type snapshotSource interface { SchedulerSnapshot(ctx context.Context, serials []string) (db.SchedulerSnapshot, error) }`
  - `type DBStore struct { ... }` implementing `Store`
  - `func NewDBStore(src snapshotSource) *DBStore`

- [ ] **Step 1: Write failing test** — `internal/scheduler/dbstore_test.go`:

```go
package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
)

type fakeSource struct{ snap db.SchedulerSnapshot }

func (f fakeSource) SchedulerSnapshot(ctx context.Context, serials []string) (db.SchedulerSnapshot, error) {
	return f.snap, nil
}

func TestDBStoreMapsRowsToSnapshot(t *testing.T) {
	last := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	src := fakeSource{snap: db.SchedulerSnapshot{
		Paused: true,
		Accounts: []db.ScheduledAccount{{ID: 3, Role: "farm", Enabled: true}},
		Tasks: []db.ScheduledTask{
			{Name: "radar_claim", CadenceSeconds: 86400, Roles: []string{"farm"}, DaysOfWeek: []int16{1, 3, 5, 6}, Enabled: true},
		},
		Runs: []db.TaskRunState{
			{AccountID: 3, TaskName: "radar_claim", LastRun: &last, ConsecutiveFailures: 2},
			{AccountID: 3, TaskName: "help_all", LastRun: nil, ConsecutiveFailures: 0},
		},
	}}

	got, err := NewDBStore(src).SchedulerSnapshot(context.Background(), []string{"s"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Paused {
		t.Error("paused not carried through")
	}
	if len(got.Tasks) != 1 || got.Tasks[0].Cadence != 24*time.Hour {
		t.Fatalf("task cadence mapping: %+v", got.Tasks)
	}
	if len(got.Tasks[0].Days) != 4 || got.Tasks[0].Days[1] != time.Wednesday {
		t.Fatalf("weekday mapping: %+v", got.Tasks[0].Days)
	}
	rs := got.Runs[RunKey{3, "radar_claim"}]
	if !rs.HasRun || !rs.LastRun.Equal(last) || rs.ConsecutiveFailures != 2 {
		t.Fatalf("run state mapping: %+v", rs)
	}
	// A nil LastRun must map to HasRun=false.
	if got.Runs[RunKey{3, "help_all"}].HasRun {
		t.Error("nil last_run should map to HasRun=false")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/scheduler/ -run TestDBStore -v`
Expected: FAIL — `NewDBStore` undefined.

- [ ] **Step 3: Implement** — `internal/scheduler/dbstore.go`:

```go
package scheduler

import (
	"context"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
)

// snapshotSource is the slice of *db.Pool the adapter needs. Narrow by
// design so tests can supply a fake without a database.
type snapshotSource interface {
	SchedulerSnapshot(ctx context.Context, serials []string) (db.SchedulerSnapshot, error)
}

// DBStore adapts the database's plain snapshot rows into the scheduler's
// richly-typed Snapshot. It is the production Store; the mapping is where
// cadence_seconds becomes a Duration and days_of_week becomes weekdays.
type DBStore struct {
	src snapshotSource
}

// NewDBStore builds a DBStore over a snapshot source (*db.Pool in production).
func NewDBStore(src snapshotSource) *DBStore {
	return &DBStore{src: src}
}

func (s *DBStore) SchedulerSnapshot(ctx context.Context, serials []string) (Snapshot, error) {
	raw, err := s.src.SchedulerSnapshot(ctx, serials)
	if err != nil {
		return Snapshot{}, err
	}

	out := Snapshot{Paused: raw.Paused, Runs: map[RunKey]RunState{}}
	for _, a := range raw.Accounts {
		out.Accounts = append(out.Accounts, Account{ID: a.ID, Role: a.Role, Enabled: a.Enabled})
	}
	for _, t := range raw.Tasks {
		days := make([]time.Weekday, 0, len(t.DaysOfWeek))
		for _, d := range t.DaysOfWeek {
			days = append(days, time.Weekday(d))
		}
		out.Tasks = append(out.Tasks, Task{
			Name:    t.Name,
			Cadence: time.Duration(t.CadenceSeconds) * time.Second,
			Roles:   t.Roles,
			Days:    days,
			Enabled: t.Enabled,
		})
	}
	for _, r := range raw.Runs {
		rs := RunState{ConsecutiveFailures: r.ConsecutiveFailures}
		if r.LastRun != nil {
			rs.LastRun = *r.LastRun
			rs.HasRun = true
		}
		out.Runs[RunKey{AccountID: r.AccountID, TaskName: r.TaskName}] = rs
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/scheduler/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/dbstore.go internal/scheduler/dbstore_test.go
git commit -m "Add DBStore adapter mapping rows to scheduler snapshot"
```

---

### Task 7: Split the radar skeleton

**Files:**
- Delete: `internal/tasks/radar.go`
- Create: `internal/tasks/radar_quick.go`, `internal/tasks/radar_claim.go`
- Modify: `internal/tasks/tasks_test.go`

**Interfaces:**
- Consumes: `collectTask` (existing in `internal/tasks/collect.go`), `Register`.
- Produces: registered task names `radar_quick` and `radar_claim`; `radar` no longer registered.

- [ ] **Step 1: Update the test to the new names** — in `internal/tasks/tasks_test.go`, change the expected-names slice and the script map. Replace:

```go
	want := []string{"daily_gather", "help_all", "mail_collect", "radar", "tech_donate"}
```

with:

```go
	want := []string{"daily_gather", "help_all", "mail_collect", "radar_claim", "radar_quick", "tech_donate"}
```

And in `skeletonScripts`, replace the `"radar"` entry:

```go
	"radar":       {"base", "base", "radar", "radar", "radar"},
```

with both split tasks (each navigates base→radar and taps):

```go
	"radar_quick": {"base", "base", "radar", "radar", "radar"},
	"radar_claim": {"base", "base", "radar", "radar", "radar"},
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tasks/ -v`
Expected: FAIL — `radar` still registered / `radar_quick` not registered (the names assertion and the missing-task lookups fail).

- [ ] **Step 3: Replace the skeleton** — delete `internal/tasks/radar.go`:

```bash
git rm internal/tasks/radar.go
```

Create `internal/tasks/radar_quick.go`:

```go
package tasks

// radar_quick runs Quick Execute on the radar screen. Worth doing every time
// the radar is seen, so it is scheduled on all days (see the tasks table).
// Skeleton: navigate to radar and tap; the real body performs Quick Execute.
func init() { Register("radar_quick", collectTask("radar", "radar_quick_button")) }
```

Create `internal/tasks/radar_claim.go`:

```go
package tasks

// radar_claim runs Claim All on the radar screen. Claims only award points
// on VS-scoring days, so the tasks table schedules it Mon/Wed/Fri/Sat and at
// most once a day — the body itself needs no calendar logic. Skeleton:
// navigate to radar and tap; the real body performs Claim All.
func init() { Register("radar_claim", collectTask("radar", "radar_claim_button")) }
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/tasks/ -v`
Expected: PASS (registry lists the six tasks; both radar skeletons run).

- [ ] **Step 5: Commit**

```bash
git add internal/tasks/
git commit -m "Split radar skeleton into radar_quick and radar_claim"
```

---

### Task 8: `agent run` command

**Files:**
- Modify: `cmd/agent/main.go`
- Modify: `CLAUDE.md`

No new unit tests: the command is thin wiring over tested parts, and its device/DB dependencies put it beyond the unit tier. Verification is build + the manual smoke lines.

**Interfaces:**
- Consumes: `scheduler.New`/`scheduler.NewDBStore`/`scheduler.Loop` (Tasks 5–6), `db.Pool.SchedulerSnapshot` (Task 1), `tasks.Get`, `runtime.New`/`runtime.Run`/`runtime.NewDBKillSwitch`/`runtime.DefaultGraph`, `vision.LoadRegistry`, `capture.New`, `transport.ListDevices`/`NewADBTransport`, `db.CaptureTargetByAccount`.

- [ ] **Step 1: Add the command to usage and dispatch** — in `cmd/agent/main.go`, add to the usage text after the `run-task` line:

```
  run       run the scheduler loop against attached devices
```

and add the switch case after `run-task`:

```go
	case "run":
		return runScheduler(ctx, cfg, os.Args[2:])
```

Add two imports: `"log/slog"` and `"github.com/tomharris/lw-manager/internal/scheduler"`. (`time`, `math/rand`, `runtime`, `tasks`, `vision`, `blob`, `capture`, `db`, `transport` are already imported by the `run-task` command; `log/slog` is *not* currently imported by this file and Step 2 needs it.)

- [ ] **Step 2: Add the executor and command** — append to `cmd/agent/main.go`:

```go
// runtimeExecutor runs one task on a real device via the task runtime. It is
// the scheduler's production Executor.
type runtimeExecutor struct {
	pool    *db.Pool
	blobs   blob.Store
	reg     *vision.Registry
	graph   *runtime.Graph
	adbPath string
}

func (e *runtimeExecutor) Execute(ctx context.Context, accountID int64, taskName string) error {
	fn, ok := tasks.Get(taskName)
	if !ok {
		return fmt.Errorf("unknown task %q", taskName)
	}
	target, err := e.pool.CaptureTargetByAccount(ctx, accountID)
	if err != nil {
		return err
	}
	tr, err := transport.NewADBTransport(ctx, transport.ADBOptions{
		ADBPath: e.adbPath,
		Serial:  target.Serial,
		Package: target.Package,
	})
	if err != nil {
		return fmt.Errorf("opening transport for device %s: %w", target.Serial, err)
	}
	defer tr.Close()

	rt, err := runtime.New(runtime.Options{
		Transport: tr,
		Registry:  e.reg,
		Graph:     e.graph,
		Kill:      runtime.NewDBKillSwitch(e.pool, accountID),
		Capture:   capture.New(e.pool, e.blobs, nil),
		AccountID: accountID,
		Rand:      rand.New(rand.NewSource(time.Now().UnixNano())),
	})
	if err != nil {
		return err
	}
	_, err = runtime.Run(ctx, e.pool, rt, taskName, fn)
	return err
}

func runScheduler(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	manifest := fs.String("templates", "templates/manifest.yaml", "template manifest path")
	tick := fs.Duration("tick", 60*time.Second, "base interval between scheduler ticks")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Load and validate the registry + graph before any DB work: a manifest
	// missing the graph's screens must fail here, loudly.
	reg, err := vision.LoadRegistry(*manifest)
	if err != nil {
		return err
	}
	graph := runtime.DefaultGraph()

	// Only drive devices attached to this host. An account registered
	// against a device that is not attached here is another agent's job.
	serials, err := transport.ListDevices(ctx, cfg.ADBPath)
	if err != nil {
		return err
	}
	if len(serials) == 0 {
		return fmt.Errorf("no devices attached; start an emulator or connect a handset")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		return err
	}

	// Fail loudly at startup if any enabled task row names a task the
	// registry does not know — the same discipline as graph validation.
	snap, err := pool.SchedulerSnapshot(ctx, serials)
	if err != nil {
		return err
	}
	for _, t := range snap.Tasks {
		if !t.Enabled {
			continue
		}
		if _, ok := tasks.Get(t.Name); !ok {
			return fmt.Errorf("tasks table enables %q but no such task is registered", t.Name)
		}
	}

	exec := &runtimeExecutor{pool: pool, blobs: blobs, reg: reg, graph: graph, adbPath: cfg.ADBPath}
	loop, err := scheduler.New(scheduler.Options{
		Store:    scheduler.NewDBStore(pool),
		Executor: exec,
		Serials:  serials,
		Tick:     *tick,
	})
	if err != nil {
		return err
	}

	slog.Info("scheduler starting", "devices", serials, "tick", tick.String())
	return loop.Run(ctx)
}
```

Note: this uses `slog.Info`, so `"log/slog"` must be in the import block (added in Step 1).

- [ ] **Step 3: Build and smoke-test the startup guard**

Run: `make build && ./bin/agent run --templates templates/manifest.yaml 2>&1 | tail -1`
Expected: loud failure — with no `templates/manifest.yaml`, the registry load fails first: `agent: vision: reading manifest templates/manifest.yaml: ...`. This is the designed endpoint for a corpus-less repo: the command exists, validates before touching a device, and starts scheduling the day the corpus and a device land.

(If a device *is* attached and a manifest exists, the loop would run; without either, the loud failure above is correct.)

- [ ] **Step 4: Update CLAUDE.md** — in the Quickstart block, after the `run-task` line, add:

```
./bin/agent run                                       # scheduler loop, all attached devices
```

In the Layout section, after the `internal/tasks` line, add:

```
internal/scheduler  cadence-driven planner + loop; decides what runs when
```

- [ ] **Step 5: Commit**

```bash
git add cmd/agent/main.go CLAUDE.md
git commit -m "Add agent run scheduler command"
```

---

### Task 9: Final verification

- [ ] **Step 1: Full unit tier with nothing running**

Run: `docker compose stop && make test && docker compose start`
Expected: PASS — the scheduler package and everything else pass device-free and DB-free (invariant #6).

- [ ] **Step 2: Integration tier from a cold database**

Run: `docker compose exec postgres psql -U lw -d postgres -c 'DROP DATABASE IF EXISTS lw_manager_test' && make test-integration`
Expected: PASS, including migration 00003 applying and the snapshot query tests.

- [ ] **Step 3: Lint and CGO check**

Run: `make lint && make verify-nocgo`
Expected: no vet findings, no gofmt diffs, clean CGO-free build.

- [ ] **Step 4: Review the diff against the spec**

Run: `git log --oneline m1-vision-core..HEAD && git diff m1-vision-core --stat`
Confirm each spec section landed: tasks table + seed (00003), `SchedulerSnapshot` query, `OfflineWindow` + wrap handling, `backoff` + derived cadence jitter, pure `Plan` with weekday gate and overdue ordering, `Loop` with the failure policy, `DBStore` adapter, radar split, `agent run` with serial scoping and the task-name startup guard.

- [ ] **Step 5: Stop**

The branch ends here. Deferred per the spec: priority/energy/preconditions, per-account cadence overrides, control-side dispatch, REST pause, per-account server timezones, and the gold-only / Quick-Execute / Claim-All on-screen behaviors (hardware-gated task bodies).

---

## Execution notes

- Tasks 2, 3, 7 are independent; 4 needs 2+3; 5 needs 4; 6 needs 1+4+5; 8 needs 1,4,5,6,7. Execute in numeric order.
- The offline-window derivation (Task 2) is the piece with the most interesting trade-offs — wrap-around and how the 5–7h duration is carved from the hash. Its tests are the contract; if executing inline in learning mode, this is the natural spot for a hand-written contribution, with the reference implementation here as the fallback.
- If a `Plan` test flakes near a boundary, suspect the offline window overlapping the chosen `now`: the test helpers advance `now` past the window on purpose, and new tests should do the same rather than assume noon is always eligible.
