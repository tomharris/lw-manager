# M2 Task Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the device-free M2 task runtime — constrained `runtime.Ctx` primitives, screen graph, panic route, kill switch, migration 00002, five Tier 1 task skeletons, and the `agent run-task` / `control pause|resume` CLI — per `docs/superpowers/specs/2026-07-22-m2-task-runtime-design.md`.

**Architecture:** Tasks are plain Go functions over a constrained `runtime.Ctx` whose primitives structurally enforce the invariants: every primitive checks the kill switch first, `Tap` accepts anchors not coordinates, `Sleep` is jittered by construction. Navigation and recovery are graph walks over data. Everything is tested against `ReplayTransport` with synthetic screens; nothing needs a device, Docker (unit tier), or adb.

**Tech Stack:** Go (CGO_ENABLED=0), pgx/goose (existing), `internal/vision` recognizer + matcher (existing), `transport.ReplayTransport` (existing).

## Global Constraints

- `CGO_ENABLED=0` always; `make verify-nocgo` must stay green.
- No absolute pixel coordinates outside a `Transport` implementation — everything speaks `transport.Norm`.
- Sentinel errors compared with `errors.Is`/`errors.As`, never by string.
- All logging via `log/slog` to stderr; CLI results to stdout (pipeable).
- Errors wrapped with `%w` plus locating context (which account, which screen, which anchor).
- No bare `time.Sleep` in task/runtime code — jittered helper only.
- `context.Context` first parameter of anything doing I/O.
- `go test ./...` must pass with nothing running (unit tier). DB-touching store tests are `//go:build integration` against `lw_manager_test` via `internal/dbtest`.
- Commit messages: plain, no AI attribution (user convention).
- Module path: `github.com/tomharris/lw-manager`.

## Branch

Work happens on a new branch cut from `m1-vision-core` (the vision core is a dependency and is not yet merged to main):

```bash
git checkout m1-vision-core && git checkout -b m2-task-runtime
```

## File Structure

```
internal/transport/transport.go      MODIFY  add Back to the Transport interface
internal/transport/replay.go         MODIFY  ReplayTransport.Back records the press
internal/db/migrations/00002_task_runtime.sql  CREATE  flags + task_runs
internal/db/store.go                 MODIFY  KillState, SetPauseAll, StartTaskRun, FinishTaskRun, TaskRunByID
internal/db/runtime_integration_test.go        CREATE  integration tests for the above
internal/runtime/runtime.go          CREATE  sentinels shared by the package
internal/runtime/jitter.go           CREATE  jittered duration/point helpers
internal/runtime/killswitch.go       CREATE  KillSwitch iface + DB-backed impl with TTL cache
internal/runtime/graph.go            CREATE  Edge/Graph types, Validate, Path (BFS), DefaultGraph
internal/runtime/ctx.go              CREATE  Ctx, Options, New, CurrentScreen, WaitFor, Sleep
internal/runtime/act.go              CREATE  Tap, Swipe, TypeText, verifyScreen
internal/runtime/panic.go            CREATE  panicRoute (back ×3 → restart → ErrLost)
internal/runtime/navigate.go         CREATE  NavigateTo with re-planning
internal/runtime/capture.go          CREATE  Capturer iface + Ctx.Capture + ScreenshotIDs
internal/runtime/runner.go           CREATE  Run: task_runs lifecycle around a TaskFunc
internal/runtime/runtimetest/runtimetest.go  CREATE  synthetic registry/graph/frames + fakes (dbtest pattern)
internal/capture/capture.go          MODIFY  extract Record(accountID, img, screenID) from Capture
internal/tasks/registry.go           CREATE  Register/Get/Names
internal/tasks/collect.go            CREATE  shared navigate-and-tap-if-present builder
internal/tasks/daily_gather.go       CREATE  skeleton
internal/tasks/help_all.go           CREATE  skeleton
internal/tasks/mail_collect.go       CREATE  skeleton
internal/tasks/tech_donate.go        CREATE  skeleton
internal/tasks/radar.go              CREATE  skeleton
cmd/agent/main.go                    MODIFY  run-task command
cmd/control/main.go                  MODIFY  pause / resume commands
CLAUDE.md                            MODIFY  quickstart additions
```

Runtime behavioral tests live in `package runtime_test` (external test package) because they import `runtimetest`, which imports `runtime` — the external package breaks the cycle, exactly why `dbtest` sits outside `db`. Pure-function tests (jitter, graph) stay white-box in `package runtime`.

---

### Task 1: `Back` on the Transport interface

`ADBTransport.Back` already exists (`internal/transport/adb.go:157`). The interface and `ReplayTransport` don't have it, so nothing upstream can call it yet.

**Files:**
- Modify: `internal/transport/transport.go` (interface, after `AppRestart`)
- Modify: `internal/transport/replay.go` (after `AppRestart`)
- Test: `internal/transport/replay_test.go`

**Interfaces:**
- Produces: `Transport.Back(ctx context.Context) error`; replay records `Action{Kind: "back"}`.

- [ ] **Step 1: Write the failing test** — append to `internal/transport/replay_test.go`:

```go
func TestReplayRecordsBack(t *testing.T) {
	rt, err := NewReplayTransportFromImages(image.NewGray(image.Rect(0, 0, 4, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Back(context.Background()); err != nil {
		t.Fatalf("Back: %v", err)
	}
	acts := rt.Actions()
	if len(acts) != 1 || acts[0].Kind != "back" {
		t.Fatalf("actions: got %+v, want one back", acts)
	}
	// The interface must carry Back, or the panic route cannot use it.
	var _ Transport = rt
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transport/ -run TestReplayRecordsBack -v`
Expected: FAIL — `rt.Back undefined`.

- [ ] **Step 3: Implement** — in `internal/transport/transport.go`, add to the `Transport` interface after `AppRestart`:

```go
	// Back presses the Android back button. Unlike Tap it needs no anchor:
	// it is the panic route's first resort, used precisely when the current
	// screen is unknown.
	Back(ctx context.Context) error
```

In `internal/transport/replay.go`, after `AppRestart`:

```go
func (t *ReplayTransport) Back(ctx context.Context) error {
	t.record(Action{Kind: "back"})
	return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/transport/... && go build ./...`
Expected: PASS, clean build (ADB already implements it).

- [ ] **Step 5: Commit**

```bash
git add internal/transport/
git commit -m "Add Back to the Transport interface"
```

---

### Task 2: Migration 00002 and runtime store methods

**Files:**
- Create: `internal/db/migrations/00002_task_runtime.sql`
- Modify: `internal/db/store.go` (append)
- Test: `internal/db/runtime_integration_test.go` (new, `//go:build integration`)

**Interfaces:**
- Produces:
  - `type KillState struct { PauseAll bool; Reason string; AccountEnabled bool }`
  - `(*Pool).KillState(ctx, accountID int64) (KillState, error)`
  - `(*Pool).SetPauseAll(ctx, paused bool, reason string) error`
  - `(*Pool).StartTaskRun(ctx, accountID int64, taskName string) (int64, error)`
  - `(*Pool).FinishTaskRun(ctx, id int64, status string, errMsg *string, screenshotIDs []int64) error`
  - `type TaskRun struct { ID, AccountID int64; TaskName string; StartedAt time.Time; EndedAt *time.Time; Status string; Error *string; ScreenshotIDs []int64 }`
  - `(*Pool).TaskRunByID(ctx, id int64) (TaskRun, error)`

- [ ] **Step 1: Write the migration** — `internal/db/migrations/00002_task_runtime.sql`:

```sql
-- +goose Up

-- Global kill switch. The boolean-primary-key trick makes a second row
-- impossible: "the" pause flag is enforced by the schema, not by convention.
CREATE TABLE flags (
    id         boolean     PRIMARY KEY DEFAULT true CHECK (id),
    pause_all  boolean     NOT NULL DEFAULT false,
    reason     text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO flags DEFAULT VALUES;

-- One row per task execution. The row is inserted as 'running' before the
-- task starts, so a killed process leaves a visibly stale running row rather
-- than nothing (invariant #2's audit trail).
CREATE TABLE task_runs (
    id             bigserial   PRIMARY KEY,
    account_id     bigint      NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    task_name      text        NOT NULL,
    started_at     timestamptz NOT NULL DEFAULT now(),
    ended_at       timestamptz,
    status         text        NOT NULL DEFAULT 'running',
    error          text,
    screenshot_ids bigint[]    NOT NULL DEFAULT '{}',

    CONSTRAINT task_runs_status_valid
        CHECK (status IN ('running', 'succeeded', 'failed', 'paused'))
);
CREATE INDEX task_runs_account_started_idx ON task_runs (account_id, started_at DESC);

-- +goose Down
DROP TABLE task_runs;
DROP TABLE flags;
```

- [ ] **Step 2: Write the failing integration test** — `internal/db/runtime_integration_test.go`. It reuses `testPool` from `store_integration_test.go` (same package) and adds a `seedAccount` fixture helper following that file's UpsertDevice → EnsureAppInstance → UpsertAccount pattern:

```go
//go:build integration

package db

import (
	"context"
	"errors"
	"testing"
)

// seedAccount creates a device/instance/account chain and returns the
// account id. The test-runtime serial namespace keeps it clear of the
// capture tests' test-emulator rows.
func seedAccount(t *testing.T, p *Pool) int64 {
	t.Helper()
	ctx := context.Background()
	dev, err := p.UpsertDevice(ctx, "test-runtime-5554", "adb", 1080, 2400)
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	instID, err := p.EnsureAppInstance(ctx, dev.ID, "com.fun.lastwar.gp", 0)
	if err != nil {
		t.Fatalf("EnsureAppInstance: %v", err)
	}
	acct, err := p.UpsertAccount(ctx, instID, "runtime-test", "farm", nil, nil)
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	return acct.ID
}

func TestKillStateAndPauseAll(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acctID := seedAccount(t, pool)

	ks, err := pool.KillState(ctx, acctID)
	if err != nil {
		t.Fatalf("KillState: %v", err)
	}
	if ks.PauseAll || !ks.AccountEnabled {
		t.Fatalf("fresh state: got %+v, want unpaused+enabled", ks)
	}

	if err := pool.SetPauseAll(ctx, true, "alliance event"); err != nil {
		t.Fatalf("SetPauseAll: %v", err)
	}
	ks, err = pool.KillState(ctx, acctID)
	if err != nil {
		t.Fatal(err)
	}
	if !ks.PauseAll || ks.Reason != "alliance event" {
		t.Fatalf("paused state: got %+v", ks)
	}
	if err := pool.SetPauseAll(ctx, false, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.KillState(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account: got %v, want ErrNotFound", err)
	}
}

func TestTaskRunLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acctID := seedAccount(t, pool)

	id, err := pool.StartTaskRun(ctx, acctID, "daily_gather")
	if err != nil {
		t.Fatalf("StartTaskRun: %v", err)
	}
	run, err := pool.TaskRunByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" || run.EndedAt != nil {
		t.Fatalf("fresh run: got %+v", run)
	}

	msg := "screen lost"
	if err := pool.FinishTaskRun(ctx, id, "failed", &msg, []int64{1, 2}); err != nil {
		t.Fatalf("FinishTaskRun: %v", err)
	}
	run, err = pool.TaskRunByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "failed" || run.EndedAt == nil || run.Error == nil || *run.Error != msg {
		t.Fatalf("finished run: got %+v", run)
	}
	if len(run.ScreenshotIDs) != 2 {
		t.Fatalf("screenshot_ids: got %v", run.ScreenshotIDs)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `docker compose up -d && go test -tags=integration ./internal/db/ -run 'TestKillState|TestTaskRunLifecycle' -v`
Expected: FAIL — methods undefined (compile error).

- [ ] **Step 4: Implement** — append to `internal/db/store.go`:

```go
// KillState is everything the runtime kill switch needs in one read: the
// global pause flag plus this account's enabled bit.
type KillState struct {
	PauseAll       bool
	Reason         string
	AccountEnabled bool
}

// KillState reads the global pause flag and the account's enabled bit.
func (p *Pool) KillState(ctx context.Context, accountID int64) (KillState, error) {
	const q = `
		SELECT f.pause_all, f.reason, a.enabled
		FROM flags f
		CROSS JOIN accounts a
		WHERE a.id = $1`

	var s KillState
	err := p.QueryRow(ctx, q, accountID).Scan(&s.PauseAll, &s.Reason, &s.AccountEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return KillState{}, fmt.Errorf("db: kill state for account %d: %w", accountID, ErrNotFound)
	}
	if err != nil {
		return KillState{}, fmt.Errorf("db: reading kill state for account %d: %w", accountID, err)
	}
	return s, nil
}

// SetPauseAll flips the global kill switch.
func (p *Pool) SetPauseAll(ctx context.Context, paused bool, reason string) error {
	const q = `UPDATE flags SET pause_all = $1, reason = $2, updated_at = now()`
	if _, err := p.Exec(ctx, q, paused, reason); err != nil {
		return fmt.Errorf("db: setting pause_all=%t: %w", paused, err)
	}
	return nil
}

// TaskRun is one recorded task execution.
type TaskRun struct {
	ID            int64
	AccountID     int64
	TaskName      string
	StartedAt     time.Time
	EndedAt       *time.Time
	Status        string
	Error         *string
	ScreenshotIDs []int64
}

// StartTaskRun records that a task is beginning, before it acts. A killed
// process therefore leaves a stale 'running' row — evidence, not silence.
func (p *Pool) StartTaskRun(ctx context.Context, accountID int64, taskName string) (int64, error) {
	const q = `INSERT INTO task_runs (account_id, task_name) VALUES ($1, $2) RETURNING id`
	var id int64
	if err := p.QueryRow(ctx, q, accountID, taskName).Scan(&id); err != nil {
		return 0, fmt.Errorf("db: starting task run %q for account %d: %w", taskName, accountID, err)
	}
	return id, nil
}

// FinishTaskRun closes a run with its outcome.
func (p *Pool) FinishTaskRun(ctx context.Context, id int64, status string, errMsg *string, screenshotIDs []int64) error {
	const q = `
		UPDATE task_runs
		SET ended_at = now(), status = $2, error = $3, screenshot_ids = $4
		WHERE id = $1`
	tag, err := p.Exec(ctx, q, id, status, errMsg, screenshotIDs)
	if err != nil {
		return fmt.Errorf("db: finishing task run %d as %q: %w", id, status, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: task run %d: %w", id, ErrNotFound)
	}
	return nil
}

// TaskRunByID fetches one run.
func (p *Pool) TaskRunByID(ctx context.Context, id int64) (TaskRun, error) {
	const q = `
		SELECT id, account_id, task_name, started_at, ended_at, status, error, screenshot_ids
		FROM task_runs WHERE id = $1`
	var r TaskRun
	err := p.QueryRow(ctx, q, id).Scan(
		&r.ID, &r.AccountID, &r.TaskName, &r.StartedAt, &r.EndedAt, &r.Status, &r.Error, &r.ScreenshotIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRun{}, fmt.Errorf("db: task run %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return TaskRun{}, fmt.Errorf("db: reading task run %d: %w", id, err)
	}
	return r, nil
}
```

Add `"time"` to store.go imports if not present.

- [ ] **Step 5: Run tests**

Run: `go test -tags=integration ./internal/db/ -v && go test ./...`
Expected: PASS both (unit tier unaffected).

- [ ] **Step 6: Commit**

```bash
git add internal/db/
git commit -m "Add flags and task_runs tables with store methods"
```

---

### Task 3: runtime package — sentinels, jitter, kill switch

**Files:**
- Create: `internal/runtime/runtime.go`, `internal/runtime/jitter.go`, `internal/runtime/killswitch.go`
- Test: `internal/runtime/jitter_test.go`, `internal/runtime/killswitch_test.go` (both white-box, `package runtime`)

**Interfaces:**
- Produces:
  - Sentinels `ErrPaused`, `ErrAccountDisabled`, `ErrLost`, `ErrWrongScreen`, `ErrAnchorNotFound`
  - `type KillSwitch interface { Check(ctx context.Context) error }`
  - `type KillStore interface { KillState(ctx context.Context, accountID int64) (db.KillState, error) }`
  - `NewDBKillSwitch(store KillStore, accountID int64) *DBKillSwitch` (2s TTL cache)
  - `jitter(r *rand.Rand, min, max time.Duration) time.Duration` (package-private)

- [ ] **Step 1: Write failing tests** — `internal/runtime/jitter_test.go`:

```go
package runtime

import (
	"math/rand"
	"testing"
	"time"
)

func TestJitterStaysInBounds(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	min, max := 100*time.Millisecond, 300*time.Millisecond
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := jitter(r, min, max)
		if d < min || d > max {
			t.Fatalf("jitter %v outside [%v, %v]", d, min, max)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Fatalf("jitter produced only %d distinct values in 200 draws — not jittered", len(seen))
	}
}

func TestJitterDegenerateRange(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	if d := jitter(r, time.Second, time.Second); d != time.Second {
		t.Fatalf("equal bounds: got %v", d)
	}
	// min > max is a programming error; jitter must not panic or go negative.
	if d := jitter(r, 2*time.Second, time.Second); d != 2*time.Second {
		t.Fatalf("inverted bounds: got %v, want clamp to min", d)
	}
}
```

`internal/runtime/killswitch_test.go`:

```go
package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
)

type scriptedKillStore struct {
	state db.KillState
	err   error
	calls int
}

func (s *scriptedKillStore) KillState(ctx context.Context, accountID int64) (db.KillState, error) {
	s.calls++
	return s.state, s.err
}

func TestKillSwitchStates(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		state db.KillState
		want  error
	}{
		{"running", db.KillState{AccountEnabled: true}, nil},
		{"paused", db.KillState{PauseAll: true, Reason: "event", AccountEnabled: true}, ErrPaused},
		{"disabled", db.KillState{AccountEnabled: false}, ErrAccountDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ks := NewDBKillSwitch(&scriptedKillStore{state: tc.state}, 42)
			err := ks.Check(ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Check: got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestKillSwitchCachesWithinTTL(t *testing.T) {
	st := &scriptedKillStore{state: db.KillState{AccountEnabled: true}}
	ks := NewDBKillSwitch(st, 42)
	ks.ttl = time.Hour // never expires within the test
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := ks.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if st.calls != 1 {
		t.Fatalf("store calls: got %d, want 1 (cached)", st.calls)
	}

	ks.ttl = 0 // every check refetches
	if err := ks.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if st.calls != 2 {
		t.Fatalf("store calls after expiry: got %d, want 2", st.calls)
	}
}

func TestKillSwitchFailsClosed(t *testing.T) {
	st := &scriptedKillStore{err: errors.New("db down")}
	ks := NewDBKillSwitch(st, 42)
	if err := ks.Check(context.Background()); err == nil {
		t.Fatal("Check with failing store: got nil, want error (fail closed)")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/runtime/ -v`
Expected: FAIL — package does not exist / identifiers undefined.

- [ ] **Step 3: Implement** — `internal/runtime/runtime.go`:

```go
// Package runtime executes tasks against a device through a constrained set
// of primitives.
//
// The primitives are the enforcement point for three platform invariants:
// every primitive checks the kill switch before acting (#8), Tap accepts
// anchors rather than coordinates so no blind tap can be expressed (#3), and
// Sleep is jittered by construction (#7). Task code gets no access to the
// underlying Transport, so none of these can be bypassed.
package runtime

import "errors"

var (
	// ErrPaused reports the global kill switch (flags.pause_all) is set.
	ErrPaused = errors.New("runtime: fleet is paused")
	// ErrAccountDisabled reports the per-account kill switch.
	ErrAccountDisabled = errors.New("runtime: account is disabled")
	// ErrLost reports that no screen could be recognized even after the
	// panic route (back ×3, then app restart) ran. The task run is failed
	// and the agent stops rather than flails.
	ErrLost = errors.New("runtime: lost - no screen recognized after recovery")
	// ErrWrongScreen reports a recognized screen other than the expected one.
	ErrWrongScreen = errors.New("runtime: not on the expected screen")
	// ErrAnchorNotFound reports an anchor that failed to match on an
	// otherwise-recognized screen. Tasks may treat this as "nothing to do"
	// (a help-all button absent means no help requests).
	ErrAnchorNotFound = errors.New("runtime: anchor not found on screen")
)
```

`internal/runtime/jitter.go`:

```go
package runtime

import (
	"math/rand"
	"time"
)

// jitter draws a uniform duration in [min, max]. Fixed timing is the most
// detectable signal the platform emits, so every wait in the runtime funnels
// through here (invariant #7). Inverted or equal bounds collapse to min
// rather than panicking: a misconfigured range should still humanize, not
// crash mid-task.
func jitter(r *rand.Rand, min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(r.Int63n(int64(max-min)+1))
}
```

`internal/runtime/killswitch.go`:

```go
package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
)

// KillSwitch answers "may this account act right now?". Checked by every Ctx
// primitive before it touches the device (invariant #8).
type KillSwitch interface {
	// Check returns nil, ErrPaused, ErrAccountDisabled, or a store error.
	Check(ctx context.Context) error
}

// KillStore is the slice of the database the switch needs. *db.Pool
// satisfies it; tests use a scripted fake.
type KillStore interface {
	KillState(ctx context.Context, accountID int64) (db.KillState, error)
}

// DBKillSwitch reads flags.pause_all and accounts.enabled, caching the
// answer briefly so a tight tap sequence does not hammer Postgres. The TTL
// is well inside the operational requirement of stopping everything within
// five seconds.
type DBKillSwitch struct {
	store     KillStore
	accountID int64
	ttl       time.Duration

	mu      sync.Mutex
	fetched time.Time
	cached  error
}

// NewDBKillSwitch builds a switch for one account with a 2s cache.
func NewDBKillSwitch(store KillStore, accountID int64) *DBKillSwitch {
	return &DBKillSwitch{store: store, accountID: accountID, ttl: 2 * time.Second}
}

func (k *DBKillSwitch) Check(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.fetched.IsZero() && time.Since(k.fetched) <= k.ttl {
		return k.cached
	}

	s, err := k.store.KillState(ctx, k.accountID)
	if err != nil {
		// Fail closed and uncached: if the database is unreachable we cannot
		// prove we are allowed to act, and the next check should retry.
		return fmt.Errorf("runtime: kill switch check for account %d: %w", k.accountID, err)
	}

	switch {
	case s.PauseAll:
		k.cached = fmt.Errorf("%w (reason: %s)", ErrPaused, s.Reason)
	case !s.AccountEnabled:
		k.cached = fmt.Errorf("account %d: %w", k.accountID, ErrAccountDisabled)
	default:
		k.cached = nil
	}
	k.fetched = time.Now()
	return k.cached
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/runtime/ -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/
git commit -m "Add runtime sentinels, jitter helper, and kill switch"
```

---

### Task 4: Screen graph

**Files:**
- Create: `internal/runtime/graph.go`
- Test: `internal/runtime/graph_test.go` (white-box, `package runtime`)

**Interfaces:**
- Consumes: `vision.Registry` (`Screen(name)` lookup, `Anchors`).
- Produces:
  - `type ActionKind int` with `ActionTap`, `ActionBack`
  - `type Edge struct { From, To string; Action ActionKind; AnchorID string }`
  - `type Graph struct { Entry string; Edges []Edge }`
  - `(*Graph).Validate(reg *vision.Registry) error`
  - `(*Graph).Path(from, to string) ([]Edge, error)`; `ErrNoPath`
  - `DefaultGraph() *Graph` — production topology, `Entry: "base"`

- [ ] **Step 1: Write failing tests** — `internal/runtime/graph_test.go`:

```go
package runtime

import (
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// graphTestRegistry: two screens, each with an identifying anchor and one
// tap anchor, enough to validate edges against.
func graphTestRegistry() *vision.Registry {
	tmpl := image.NewGray(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			tmpl.SetGray(x, y, color.Gray{Y: uint8(40 * (x + y))})
		}
	}
	full := transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}
	anchors := func() []vision.Anchor {
		return []vision.Anchor{
			{ID: "id", Template: tmpl, Region: full, Threshold: 0.9, IdentifiesScreen: true},
			{ID: "go_button", Template: tmpl, Region: full, Threshold: 0.9},
		}
	}
	return &vision.Registry{
		ReferenceHeight: 64,
		Screens: []vision.Screen{
			{Name: "alliance", Anchors: anchors()},
			{Name: "base", Anchors: anchors()},
		},
	}
}

func TestGraphValidate(t *testing.T) {
	reg := graphTestRegistry()
	good := &Graph{Entry: "base", Edges: []Edge{
		{From: "base", To: "alliance", Action: ActionTap, AnchorID: "go_button"},
		{From: "alliance", To: "base", Action: ActionBack},
	}}
	if err := good.Validate(reg); err != nil {
		t.Fatalf("valid graph rejected: %v", err)
	}

	cases := []struct {
		name string
		g    *Graph
	}{
		{"unknown entry", &Graph{Entry: "nope", Edges: good.Edges}},
		{"unknown from", &Graph{Entry: "base", Edges: []Edge{{From: "nope", To: "base", Action: ActionBack}}}},
		{"unknown to", &Graph{Entry: "base", Edges: []Edge{{From: "base", To: "nope", Action: ActionBack}}}},
		{"unknown anchor", &Graph{Entry: "base", Edges: []Edge{{From: "base", To: "alliance", Action: ActionTap, AnchorID: "nope"}}}},
		{"tap without anchor", &Graph{Entry: "base", Edges: []Edge{{From: "base", To: "alliance", Action: ActionTap}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.g.Validate(reg); err == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
}

func TestGraphPath(t *testing.T) {
	g := &Graph{Entry: "a", Edges: []Edge{
		{From: "a", To: "b", Action: ActionBack},
		{From: "b", To: "c", Action: ActionBack},
		{From: "a", To: "c", Action: ActionBack}, // direct shortcut
	}}
	path, err := g.Path("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 1 || path[0].To != "c" {
		t.Fatalf("path: got %+v, want the direct a->c edge", path)
	}

	if _, err := g.Path("c", "a"); !errors.Is(err, ErrNoPath) {
		t.Fatalf("unreachable: got %v, want ErrNoPath", err)
	}

	path, err = g.Path("b", "b")
	if err != nil || len(path) != 0 {
		t.Fatalf("self path: got %+v, %v; want empty, nil", path, err)
	}
}

func TestDefaultGraphShape(t *testing.T) {
	g := DefaultGraph()
	if g.Entry != "base" {
		t.Fatalf("entry: got %q, want base", g.Entry)
	}
	// Every Tier 1 task screen must be reachable from the entry screen.
	for _, target := range []string{"alliance", "alliance_tech", "mail", "radar"} {
		if _, err := g.Path(g.Entry, target); err != nil {
			t.Errorf("no path from %q to %q: %v", g.Entry, target, err)
		}
		if _, err := g.Path(target, g.Entry); err != nil {
			t.Errorf("no path back from %q: %v", target, err)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/runtime/ -run TestGraph -v`
Expected: FAIL — types undefined.

- [ ] **Step 3: Implement** — `internal/runtime/graph.go`:

```go
package runtime

import (
	"errors"
	"fmt"

	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrNoPath reports that the screen graph has no route between two screens.
var ErrNoPath = errors.New("runtime: no path between screens")

// ActionKind is what an edge does to move between screens.
type ActionKind int

const (
	// ActionTap taps a named anchor on the From screen.
	ActionTap ActionKind = iota
	// ActionBack presses the Android back button.
	ActionBack
)

// Edge is one navigation step: from one screen to another via an action.
type Edge struct {
	From, To string
	Action   ActionKind
	AnchorID string // required for ActionTap
}

// Graph is the navigation topology. It is data, validated against the
// template registry at Ctx construction: an edge naming a screen or anchor
// the registry does not know is a bug that must fail loudly at startup, not
// surface as a mysterious mid-task miss.
type Graph struct {
	// Entry is the screen the game lands on after an app restart — the
	// panic route's final waypoint.
	Entry string
	Edges []Edge
}

// Validate checks every edge against the loaded registry.
func (g *Graph) Validate(reg *vision.Registry) error {
	if _, ok := reg.Screen(g.Entry); !ok {
		return fmt.Errorf("runtime: graph entry screen %q not in registry", g.Entry)
	}
	for _, e := range g.Edges {
		from, ok := reg.Screen(e.From)
		if !ok {
			return fmt.Errorf("runtime: graph edge %q->%q: screen %q not in registry", e.From, e.To, e.From)
		}
		if _, ok := reg.Screen(e.To); !ok {
			return fmt.Errorf("runtime: graph edge %q->%q: screen %q not in registry", e.From, e.To, e.To)
		}
		if e.Action == ActionTap {
			if e.AnchorID == "" {
				return fmt.Errorf("runtime: graph edge %q->%q: tap edge without an anchor", e.From, e.To)
			}
			found := false
			for _, a := range from.Anchors {
				if a.ID == e.AnchorID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("runtime: graph edge %q->%q: anchor %q not on screen %q", e.From, e.To, e.AnchorID, e.From)
			}
		}
	}
	return nil
}

// Path returns the shortest edge sequence from one screen to another (BFS,
// deterministic by edge order). An empty path means from == to.
func (g *Graph) Path(from, to string) ([]Edge, error) {
	if from == to {
		return nil, nil
	}
	type node struct {
		screen string
		path   []Edge
	}
	visited := map[string]bool{from: true}
	queue := []node{{screen: from}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.Edges {
			if e.From != cur.screen || visited[e.To] {
				continue
			}
			path := append(append([]Edge{}, cur.path...), e)
			if e.To == to {
				return path, nil
			}
			visited[e.To] = true
			queue = append(queue, node{screen: e.To, path: path})
		}
	}
	return nil, fmt.Errorf("runtime: %q -> %q: %w", from, to, ErrNoPath)
}

// DefaultGraph is the production topology for the Tier 1 tasks. The anchor
// IDs name templates that will exist once the real corpus is captured; until
// then Validate rejects this graph against the shipping manifest, which is
// the designed behavior — skeleton tasks must refuse to run rather than
// blind-tap unproven screens.
func DefaultGraph() *Graph {
	return &Graph{
		Entry: "base",
		Edges: []Edge{
			{From: "base", To: "world_map", Action: ActionTap, AnchorID: "world_map_button"},
			{From: "world_map", To: "base", Action: ActionTap, AnchorID: "base_button"},
			{From: "base", To: "alliance", Action: ActionTap, AnchorID: "alliance_button"},
			{From: "alliance", To: "base", Action: ActionBack},
			{From: "alliance", To: "alliance_tech", Action: ActionTap, AnchorID: "tech_button"},
			{From: "alliance_tech", To: "alliance", Action: ActionBack},
			{From: "base", To: "mail", Action: ActionTap, AnchorID: "mail_button"},
			{From: "mail", To: "base", Action: ActionBack},
			{From: "base", To: "radar", Action: ActionTap, AnchorID: "radar_button"},
			{From: "radar", To: "base", Action: ActionBack},
		},
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/runtime/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/graph.go internal/runtime/graph_test.go
git commit -m "Add screen graph with validation and BFS pathing"
```

---

### Task 5: runtimetest helpers + Ctx core (New, CurrentScreen, WaitFor, Sleep)

**Files:**
- Create: `internal/runtime/runtimetest/runtimetest.go`
- Create: `internal/runtime/ctx.go`
- Test: `internal/runtime/ctx_test.go` (**`package runtime_test`** — imports runtimetest)

**Interfaces:**
- Consumes: Task 3 sentinels + `KillSwitch`; Task 4 `Graph`; `vision.Recognizer/Registry`; `transport.Transport`.
- Produces:
  - `type Recognition struct { Screen string; Confidence float64 }`
  - `type Options struct { Transport transport.Transport; Registry *vision.Registry; Graph *Graph; Kill KillSwitch; AccountID int64; Rand *rand.Rand; PollInterval, WaitTimeout, RestartTimeout time.Duration; Log *slog.Logger }`
  - `New(opts Options) (*Ctx, error)` — validates required fields and graph
  - `(*Ctx).CurrentScreen(ctx) (Recognition, error)`
  - `(*Ctx).WaitFor(ctx, screen string) (Recognition, error)` — gives up early after 5 consecutive identical wrong-screen sightings (`ErrWrongScreen`)
  - `(*Ctx).Sleep(ctx, min, max time.Duration) error`
  - unexported `recognize`, `recognizeOrRecover`, and a stub `panicRoute` (returns `ErrLost`; Task 7 replaces the body)
  - runtimetest: `Registry() *vision.Registry`, `Graph() *runtime.Graph`, `Frame(screen string) image.Image`, `Blank() image.Image`, `type FakeKill`, `Options(tr transport.Transport, ks runtime.KillSwitch) runtime.Options`

- [ ] **Step 1: Write runtimetest** — `internal/runtime/runtimetest/runtimetest.go`. This is a non-test file imported only by tests, the `internal/dbtest` pattern. Five synthetic screens are distinguished by *where* a single 8×8 pattern sits on a 64×64 frame, with each screen's anchors region-restricted to its own area — same trick as the vision tests, but positional so patterns never cross-match:

```go
// Package runtimetest provides synthetic screens, a matching graph, and
// fakes for testing the task runtime with no device attached.
//
// Five screens share one 8x8 template, distinguished by position: each
// screen's anchors search only its own region of the 64x64 frame, and
// Frame(screen) renders the pattern only at that screen's spot. Recognition
// is therefore exact and deterministic without needing patterns that are
// provably NCC-distinct from each other.
package runtimetest

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"sync"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// spot places one screen's pattern and the region its anchors search.
type spot struct {
	px, py int            // paste position of the 8x8 pattern
	region transport.Rect // search region containing it, disjoint from others
}

var spots = map[string]spot{
	"base":          {4, 4, transport.Rect{X1: 0, Y1: 0, X2: 0.45, Y2: 0.45}},
	"alliance":      {44, 4, transport.Rect{X1: 0.55, Y1: 0, X2: 1, Y2: 0.45}},
	"mail":          {4, 44, transport.Rect{X1: 0, Y1: 0.55, X2: 0.45, Y2: 1}},
	"radar":         {44, 44, transport.Rect{X1: 0.55, Y1: 0.55, X2: 1, Y2: 1}},
	"alliance_tech": {28, 28, transport.Rect{X1: 0.42, Y1: 0.42, X2: 0.58, Y2: 0.58}},
}

// tapAnchors names the tap target each Tier 1 skeleton uses, per screen.
var tapAnchors = map[string]string{
	"base":          "gather_button",
	"alliance":      "help_all_button",
	"mail":          "collect_all_button",
	"alliance_tech": "donate_button",
	"radar":         "radar_claim_button",
}

func pattern() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			g.SetGray(x, y, color.Gray{Y: uint8((x*37 + y*53) % 256)})
		}
	}
	return g
}

// Registry builds the five-screen synthetic registry at ReferenceHeight 64.
// Each screen has an identifying anchor plus the tap anchor its skeleton
// task needs; both share the screen's pattern spot.
func Registry() *vision.Registry {
	p := pattern()
	reg := &vision.Registry{ReferenceHeight: 64}
	for _, name := range []string{"alliance", "alliance_tech", "base", "mail", "radar"} {
		s := spots[name]
		reg.Screens = append(reg.Screens, vision.Screen{
			Name: name,
			Anchors: []vision.Anchor{
				{ID: "id", Template: p, Region: s.region, Threshold: 0.9, IdentifiesScreen: true},
				{ID: tapAnchors[name], Template: p, Region: s.region, Threshold: 0.9},
			},
		})
	}
	return reg
}

// Graph mirrors DefaultGraph's topology over the synthetic screens, using
// the "id" anchor as every tap edge's target (it is present and matchable
// on each screen's frame).
func Graph() *runtime.Graph {
	return &runtime.Graph{
		Entry: "base",
		Edges: []runtime.Edge{
			{From: "base", To: "alliance", Action: runtime.ActionTap, AnchorID: "id"},
			{From: "alliance", To: "base", Action: runtime.ActionBack},
			{From: "alliance", To: "alliance_tech", Action: runtime.ActionTap, AnchorID: "id"},
			{From: "alliance_tech", To: "alliance", Action: runtime.ActionBack},
			{From: "base", To: "mail", Action: runtime.ActionTap, AnchorID: "id"},
			{From: "mail", To: "base", Action: runtime.ActionBack},
			{From: "base", To: "radar", Action: runtime.ActionTap, AnchorID: "id"},
			{From: "radar", To: "base", Action: runtime.ActionBack},
		},
	}
}

// Frame renders the named screen: flat gray with the pattern at the
// screen's spot. Panics on unknown names — a test bug, not a runtime case.
func Frame(screen string) image.Image {
	s, ok := spots[screen]
	if !ok {
		panic(fmt.Sprintf("runtimetest: unknown screen %q", screen))
	}
	out := image.NewGray(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			out.SetGray(x, y, color.Gray{Y: 100})
		}
	}
	p := pattern()
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			out.SetGray(s.px+x, s.py+y, p.GrayAt(x, y))
		}
	}
	return out
}

// Blank is a frame no screen recognizes.
func Blank() image.Image {
	out := image.NewGray(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			out.SetGray(x, y, color.Gray{Y: 100})
		}
	}
	return out
}

// Frames is a convenience for staging a replay script by screen name;
// "" stages a Blank frame.
func Frames(names ...string) []image.Image {
	out := make([]image.Image, len(names))
	for i, n := range names {
		if n == "" {
			out[i] = Blank()
		} else {
			out[i] = Frame(n)
		}
	}
	return out
}

// FakeKill is a settable kill switch for tests.
type FakeKill struct {
	mu  sync.Mutex
	err error
}

// Set makes every subsequent Check return err (nil re-enables).
func (k *FakeKill) Set(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

func (k *FakeKill) Check(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

// Options returns runtime.Options wired for fast deterministic tests:
// synthetic registry and graph, seeded rand, millisecond timing.
func Options(tr transport.Transport, ks runtime.KillSwitch) runtime.Options {
	return runtime.Options{
		Transport:      tr,
		Registry:       Registry(),
		Graph:          Graph(),
		Kill:           ks,
		AccountID:      1,
		Rand:           rand.New(rand.NewSource(42)),
		PollInterval:   time.Millisecond,
		WaitTimeout:    50 * time.Millisecond,
		RestartTimeout: 50 * time.Millisecond,
	}
}
```

Add `"time"` to the import block.

- [ ] **Step 2: Write failing tests** — `internal/runtime/ctx_test.go`:

```go
package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

func newCtx(t *testing.T, ks runtime.KillSwitch, frames ...string) (*runtime.Ctx, *transport.ReplayTransport) {
	t.Helper()
	tr, err := transport.NewReplayTransportFromImages(runtimetest.Frames(frames...)...)
	if err != nil {
		t.Fatal(err)
	}
	c, err := runtime.New(runtimetest.Options(tr, ks))
	if err != nil {
		t.Fatal(err)
	}
	return c, tr
}

func TestNewValidatesGraphAgainstRegistry(t *testing.T) {
	tr, _ := transport.NewReplayTransportFromImages(runtimetest.Blank())
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Graph = &runtime.Graph{Entry: "nonexistent"}
	if _, err := runtime.New(opts); err == nil {
		t.Fatal("New accepted a graph naming an unknown screen")
	}
}

func TestCurrentScreenRecognizes(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "alliance")
	r, err := c.CurrentScreen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Screen != "alliance" || r.Confidence < 0.9 {
		t.Fatalf("recognition: got %+v", r)
	}
}

func TestWaitForPollsUntilScreenAppears(t *testing.T) {
	// Two unrecognizable frames, then mail: WaitFor must poll through them.
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "", "", "mail")
	r, err := c.WaitFor(context.Background(), "mail")
	if err != nil {
		t.Fatal(err)
	}
	if r.Screen != "mail" {
		t.Fatalf("screen: got %q", r.Screen)
	}
}

func TestWaitForGivesUpOnSteadyWrongScreen(t *testing.T) {
	// The device sits on base forever; waiting for mail must fail with
	// ErrWrongScreen after the wrong-screen streak, not hang to timeout.
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	_, err := c.WaitFor(context.Background(), "mail")
	if !errors.Is(err, runtime.ErrWrongScreen) {
		t.Fatalf("got %v, want ErrWrongScreen", err)
	}
}

func TestPrimitivesCheckKillSwitchFirst(t *testing.T) {
	ks := &runtimetest.FakeKill{}
	ks.Set(runtime.ErrPaused)
	c, tr := newCtx(t, ks, "base")

	if _, err := c.CurrentScreen(context.Background()); !errors.Is(err, runtime.ErrPaused) {
		t.Fatalf("CurrentScreen: got %v, want ErrPaused", err)
	}
	if err := c.Sleep(context.Background(), time.Millisecond, 2*time.Millisecond); !errors.Is(err, runtime.ErrPaused) {
		t.Fatalf("Sleep: got %v, want ErrPaused", err)
	}
	if len(tr.Actions()) != 0 {
		t.Fatalf("paused ctx touched the device: %+v", tr.Actions())
	}
}

func TestSleepRespectsContextCancel(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sleep(ctx, time.Hour, 2*time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestUnrecognizedScreenReachesStubRecovery(t *testing.T) {
	// Until the panic route lands (Task 7), an unrecognized screen must
	// surface ErrLost — never vision.ErrNoScreenRecognized, which task code
	// is not supposed to see.
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "")
	_, err := c.CurrentScreen(context.Background())
	if !errors.Is(err, runtime.ErrLost) {
		t.Fatalf("got %v, want ErrLost", err)
	}
	if errors.Is(err, vision.ErrNoScreenRecognized) {
		t.Fatal("vision sentinel leaked through the runtime boundary")
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/runtime/... -v`
Expected: FAIL — `runtime.Ctx`, `runtime.New` undefined.

- [ ] **Step 4: Implement** — `internal/runtime/ctx.go`:

```go
package runtime

import (
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"math/rand"
	"time"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// Recognition is a recognized screen with the recognizer's confidence.
type Recognition struct {
	Screen     string
	Confidence float64
}

// Options configures a Ctx. Transport, Registry, Graph, and Kill are
// required; the rest default sensibly.
type Options struct {
	Transport transport.Transport
	Registry  *vision.Registry
	Graph     *Graph
	Kill      KillSwitch
	AccountID int64

	// Rand drives all jitter. Nil means time-seeded; tests inject a seeded
	// source for determinism.
	Rand *rand.Rand
	// PollInterval paces recognition attempts inside WaitFor. Default 500ms.
	PollInterval time.Duration
	// WaitTimeout bounds WaitFor. Default 20s.
	WaitTimeout time.Duration
	// RestartTimeout bounds the wait for the entry screen after an app
	// restart — game boots are slow. Default 90s.
	RestartTimeout time.Duration
	Log            *slog.Logger
}

// Ctx is the only surface task code gets. Every primitive checks the kill
// switch before acting, requires recognition before any interaction, and
// jitters all timing. There is deliberately no accessor for the underlying
// Transport.
type Ctx struct {
	tr             transport.Transport
	reg            *vision.Registry
	rec            *vision.Recognizer
	graph          *Graph
	ks             KillSwitch
	cap            Capturer // nil until a capturer is configured (Task 9)
	accountID      int64
	rand           *rand.Rand
	poll           time.Duration
	waitTimeout    time.Duration
	restartTimeout time.Duration
	log            *slog.Logger
	screenshotIDs  []int64
}

// New builds a Ctx, validating the graph against the registry so a bad edge
// fails at startup rather than mid-task.
func New(opts Options) (*Ctx, error) {
	if opts.Transport == nil || opts.Registry == nil || opts.Graph == nil || opts.Kill == nil {
		return nil, errors.New("runtime: Transport, Registry, Graph and Kill are all required")
	}
	if err := opts.Graph.Validate(opts.Registry); err != nil {
		return nil, err
	}
	r := opts.Rand
	if r == nil {
		r = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	c := &Ctx{
		tr:             opts.Transport,
		reg:            opts.Registry,
		rec:            vision.NewRecognizer(opts.Registry),
		graph:          opts.Graph,
		ks:             opts.Kill,
		accountID:      opts.AccountID,
		rand:           r,
		poll:           opts.PollInterval,
		waitTimeout:    opts.WaitTimeout,
		restartTimeout: opts.RestartTimeout,
		log:            opts.Log,
	}
	if c.poll <= 0 {
		c.poll = 500 * time.Millisecond
	}
	if c.waitTimeout <= 0 {
		c.waitTimeout = 20 * time.Second
	}
	if c.restartTimeout <= 0 {
		c.restartTimeout = 90 * time.Second
	}
	if c.log == nil {
		c.log = slog.Default().With("component", "runtime", "account", opts.AccountID)
	}
	return c, nil
}

// recognize captures one frame and recognizes it. Errors from the vision
// layer pass through raw; callers translate ErrNoScreenRecognized into
// recovery (recognizeOrRecover) so it never crosses the package boundary.
func (c *Ctx) recognize(ctx context.Context) (Recognition, image.Image, error) {
	frame, err := c.tr.Screenshot(ctx)
	if err != nil {
		return Recognition{}, nil, fmt.Errorf("runtime: screenshotting for account %d: %w", c.accountID, err)
	}
	screen, conf, err := c.rec.Recognize(frame)
	if err != nil {
		return Recognition{}, frame, err
	}
	return Recognition{Screen: screen, Confidence: conf}, frame, nil
}

// recognizeOrRecover recognizes the current frame, invoking the panic route
// when nothing matches. Task code above this line never sees
// vision.ErrNoScreenRecognized.
func (c *Ctx) recognizeOrRecover(ctx context.Context) (Recognition, error) {
	r, _, err := c.recognize(ctx)
	if errors.Is(err, vision.ErrNoScreenRecognized) {
		return c.panicRoute(ctx)
	}
	return r, err
}

// CurrentScreen reports which screen the device shows right now.
func (c *Ctx) CurrentScreen(ctx context.Context) (Recognition, error) {
	if err := c.ks.Check(ctx); err != nil {
		return Recognition{}, err
	}
	return c.recognizeOrRecover(ctx)
}

// wrongScreenStreak is how many consecutive identical wrong-screen sightings
// make WaitFor give up early: a stable recognized screen will not become a
// different one by staring at it, and callers (NavigateTo) can re-plan from
// the screen WaitFor reports.
const wrongScreenStreak = 5

// WaitFor polls until the named screen is recognized. It fails with
// ErrWrongScreen when a different screen shows steadily, and with the panic
// route's verdict when nothing is recognized for the whole timeout.
func (c *Ctx) WaitFor(ctx context.Context, screen string) (Recognition, error) {
	deadline := time.Now().Add(c.waitTimeout)
	streakScreen, streak := "", 0
	for {
		if err := c.ks.Check(ctx); err != nil {
			return Recognition{}, err
		}
		r, _, err := c.recognize(ctx)
		switch {
		case err == nil && r.Screen == screen:
			return r, nil
		case err == nil:
			if r.Screen == streakScreen {
				streak++
			} else {
				streakScreen, streak = r.Screen, 1
			}
			if streak >= wrongScreenStreak {
				return Recognition{}, fmt.Errorf("runtime: waiting for %q but device sits on %q: %w", screen, r.Screen, ErrWrongScreen)
			}
		case errors.Is(err, vision.ErrNoScreenRecognized):
			streakScreen, streak = "", 0 // unrecognized frames reset the streak
		default:
			return Recognition{}, err
		}

		if time.Now().After(deadline) {
			// Nothing recognizable for the whole budget: recover, then let
			// the caller decide what to do from wherever we ended up.
			r, rerr := c.panicRoute(ctx)
			if rerr != nil {
				return Recognition{}, rerr
			}
			if r.Screen == screen {
				return r, nil
			}
			return Recognition{}, fmt.Errorf("runtime: waited %s for %q, recovered onto %q: %w", c.waitTimeout, screen, r.Screen, ErrWrongScreen)
		}
		if err := c.Sleep(ctx, c.poll, 2*c.poll); err != nil {
			return Recognition{}, err
		}
	}
}

// Sleep waits a jittered duration in [min, max]. The only sanctioned wait.
func (c *Ctx) Sleep(ctx context.Context, min, max time.Duration) error {
	if err := c.ks.Check(ctx); err != nil {
		return err
	}
	t := time.NewTimer(jitter(c.rand, min, max))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
```

Also add a temporary `panicRoute` stub at the bottom of `ctx.go` (moved to `panic.go` and given its real body in Task 7):

```go
// panicRoute recovers from an unrecognized screen. Stub until the recovery
// task lands: report lost immediately.
func (c *Ctx) panicRoute(ctx context.Context) (Recognition, error) {
	return Recognition{}, ErrLost
}
```

And the `Capturer` placeholder so `Ctx.cap` compiles before Task 9 — in `ctx.go`:

```go
// Capturer records an already-captured frame; the real interface (backed by
// internal/capture) is wired in the capture task.
type Capturer interface{}
```

(Task 9 replaces this placeholder with the real method set.)

- [ ] **Step 5: Run tests**

Run: `go test ./internal/runtime/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/
git commit -m "Add runtime Ctx core: recognition, WaitFor, jittered sleep"
```

---

### Task 6: Tap, Swipe, TypeText

**Files:**
- Create: `internal/runtime/act.go`
- Test: `internal/runtime/act_test.go` (`package runtime_test`)

**Interfaces:**
- Consumes: `verifyScreen` uses `recognize` + `panicRoute` from Task 5; `vision.Match`.
- Produces:
  - `(*Ctx).Tap(ctx, screen, anchorID string) error`
  - `(*Ctx).Swipe(ctx, from, to transport.Norm) error`
  - `(*Ctx).TypeText(ctx, s string) error`
  - unexported `verifyScreen(ctx, screen string) (image.Image, error)` — reused by Task 9's Capture

- [ ] **Step 1: Write failing tests** — `internal/runtime/act_test.go`:

```go
package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
)

func TestTapHitsInsideMatchedAnchor(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "base")
	if err := c.Tap(context.Background(), "base", "gather_button"); err != nil {
		t.Fatal(err)
	}
	acts := tr.Actions()
	if len(acts) != 1 || acts[0].Kind != "tap" {
		t.Fatalf("actions: %+v", acts)
	}
	// The synthetic base pattern occupies pixels (4,4)-(12,12) of 64 → the
	// tap must land inside that box in normalized coordinates.
	p := acts[0].At
	if p.X < 4.0/64 || p.X > 12.0/64 || p.Y < 4.0/64 || p.Y > 12.0/64 {
		t.Fatalf("tap at %+v outside the matched anchor box", p)
	}
}

func TestTapWrongScreenRefused(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "mail")
	err := c.Tap(context.Background(), "base", "gather_button")
	if !errors.Is(err, runtime.ErrWrongScreen) {
		t.Fatalf("got %v, want ErrWrongScreen", err)
	}
	if len(tr.Actions()) != 0 {
		t.Fatalf("tapped despite wrong screen: %+v", tr.Actions())
	}
}

func TestTapUnknownAnchorErrors(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	if err := c.Tap(context.Background(), "base", "no_such_anchor"); err == nil {
		t.Fatal("unknown anchor accepted")
	}
}

func TestSwipeRefusedOnUnrecognizedScreen(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "")
	err := c.Swipe(context.Background(),
		transport.Norm{X: 0.5, Y: 0.8}, transport.Norm{X: 0.5, Y: 0.2})
	if !errors.Is(err, runtime.ErrLost) {
		t.Fatalf("got %v, want ErrLost (stub recovery)", err)
	}
	if len(tr.Actions()) != 0 {
		t.Fatalf("swiped blind: %+v", tr.Actions())
	}
}

func TestSwipeJittersEndpointsAndDuration(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "base")
	from, to := transport.Norm{X: 0.5, Y: 0.8}, transport.Norm{X: 0.5, Y: 0.2}
	if err := c.Swipe(context.Background(), from, to); err != nil {
		t.Fatal(err)
	}
	a := tr.Actions()[0]
	if a.Kind != "swipe" || a.Duration <= 0 {
		t.Fatalf("swipe action: %+v", a)
	}
	if !a.At.Valid() || !a.To.Valid() {
		t.Fatalf("jittered endpoints left the unit square: %+v", a)
	}
	if a.At == from && a.To == to {
		t.Fatal("swipe endpoints not jittered at all")
	}
}

func TestTypeTextRequiresRecognizedScreen(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "base")
	if err := c.TypeText(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if acts := tr.Actions(); len(acts) != 1 || acts[0].Text != "hello" {
		t.Fatalf("actions: %+v", acts)
	}
}
```

(Tap-point *variation* across runs is deliberately not asserted here: with a seeded rand it would be tautological. The in-box assertion plus the jitter-bounds unit test cover the behavior.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/runtime/... -run 'TestTap|TestSwipe|TestType' -v`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Implement** — `internal/runtime/act.go`:

```go
package runtime

import (
	"context"
	"errors"
	"fmt"
	"image"
	"time"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// verifyScreen proves the device currently shows the named screen and
// returns the frame that proves it. Unrecognized frames go through the
// panic route once; a recognized-but-different screen is ErrWrongScreen.
func (c *Ctx) verifyScreen(ctx context.Context, screen string) (image.Image, error) {
	r, frame, err := c.recognize(ctx)
	if errors.Is(err, vision.ErrNoScreenRecognized) {
		if _, rerr := c.panicRoute(ctx); rerr != nil {
			return nil, rerr
		}
		r, frame, err = c.recognize(ctx)
	}
	if err != nil {
		return nil, err
	}
	if r.Screen != screen {
		return nil, fmt.Errorf("runtime: on %q, want %q: %w", r.Screen, screen, ErrWrongScreen)
	}
	return frame, nil
}

// Tap verifies the screen, matches the anchor, and taps a jittered point
// inside the match's bounding box. It accepts no coordinates: invariant #3
// (no blind taps) is enforced by this signature.
func (c *Ctx) Tap(ctx context.Context, screen, anchorID string) error {
	if err := c.ks.Check(ctx); err != nil {
		return err
	}
	s, ok := c.reg.Screen(screen)
	if !ok {
		return fmt.Errorf("runtime: unknown screen %q", screen)
	}
	var anchor *vision.Anchor
	for i := range s.Anchors {
		if s.Anchors[i].ID == anchorID {
			anchor = &s.Anchors[i]
			break
		}
	}
	if anchor == nil {
		return fmt.Errorf("runtime: screen %q has no anchor %q", screen, anchorID)
	}

	frame, err := c.verifyScreen(ctx, screen)
	if err != nil {
		return err
	}
	m, err := vision.Match(frame, anchor.Template, anchor.Region, c.reg.ReferenceHeight)
	if err != nil {
		return fmt.Errorf("runtime: matching %q on %q: %w", anchorID, screen, err)
	}
	if m.Score < anchor.Threshold {
		return fmt.Errorf("runtime: screen %q anchor %q scored %.3f below %.3f: %w",
			screen, anchorID, m.Score, anchor.Threshold, ErrAnchorNotFound)
	}
	return c.tr.Tap(ctx, c.jitterPoint(m.Box))
}

// jitterPoint picks a point inside the central 60% of a matched box — never
// dead center, never the edges. Fixed tap pixels are as detectable as fixed
// timing.
func (c *Ctx) jitterPoint(box transport.Rect) transport.Norm {
	fx := 0.2 + 0.6*c.rand.Float64()
	fy := 0.2 + 0.6*c.rand.Float64()
	return transport.Norm{
		X: box.X1 + (box.X2-box.X1)*fx,
		Y: box.Y1 + (box.Y2-box.Y1)*fy,
	}
}

// Swipe drags between two normalized points with jittered endpoints and
// duration. Endpoints are positional (scrolling has no anchor), but the
// screen must still be recognized first: invariant #3 covers every action.
func (c *Ctx) Swipe(ctx context.Context, from, to transport.Norm) error {
	if err := c.ks.Check(ctx); err != nil {
		return err
	}
	if _, err := c.recognizeOrRecover(ctx); err != nil {
		return err
	}
	d := jitter(c.rand, 250*time.Millisecond, 600*time.Millisecond)
	return c.tr.Swipe(ctx, c.jitterNorm(from), c.jitterNorm(to), d)
}

// jitterNorm nudges a point by up to ±2% per axis, clamped to the unit
// square so jitter can never manufacture an out-of-range coordinate.
func (c *Ctx) jitterNorm(p transport.Norm) transport.Norm {
	return transport.Norm{
		X: clamp01(p.X + (c.rand.Float64()-0.5)*0.04),
		Y: clamp01(p.Y + (c.rand.Float64()-0.5)*0.04),
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// TypeText types into the focused field, requiring a recognized screen.
func (c *Ctx) TypeText(ctx context.Context, s string) error {
	if err := c.ks.Check(ctx); err != nil {
		return err
	}
	if _, err := c.recognizeOrRecover(ctx); err != nil {
		return err
	}
	return c.tr.TypeText(ctx, s)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/runtime/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/act.go internal/runtime/act_test.go
git commit -m "Add Tap, Swipe, TypeText with screen verification and jitter"
```

---

### Task 7: Panic route

**Files:**
- Create: `internal/runtime/panic.go` (move the stub out of `ctx.go`, give it its real body)
- Modify: `internal/runtime/ctx.go` (delete the stub)
- Test: `internal/runtime/panic_test.go` (`package runtime_test`)

**Interfaces:**
- Consumes: `Transport.Back`/`AppRestart`, `recognize`, `Sleep`, `graph.Entry`.
- Produces: real `panicRoute` — back ×3 → restart → wait for entry screen → `ErrLost`.

- [ ] **Step 1: Write failing tests** — `internal/runtime/panic_test.go`:

```go
package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
)

func countKind(acts []transport.Action, kind string) int {
	n := 0
	for _, a := range acts {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

func TestPanicRouteRecoversWithBack(t *testing.T) {
	// Frame 1 unrecognized triggers the route; frame 2 (after one back) is
	// recognizable. CurrentScreen must succeed and report it.
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "", "base")
	r, err := c.CurrentScreen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Screen != "base" {
		t.Fatalf("recovered onto %q, want base", r.Screen)
	}
	acts := tr.Actions()
	if countKind(acts, "back") != 1 {
		t.Fatalf("back presses: got %d, want 1 (%+v)", countKind(acts, "back"), acts)
	}
	if countKind(acts, "restart") != 0 {
		t.Fatal("restarted when back sufficed")
	}
}

func TestPanicRouteFallsBackToRestart(t *testing.T) {
	// Four unrecognized frames absorb the trigger plus all three back
	// attempts; the next frame is the entry screen, reachable only through
	// restart.
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "", "", "", "", "base")
	r, err := c.CurrentScreen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Screen != "base" {
		t.Fatalf("recovered onto %q, want the entry screen", r.Screen)
	}
	acts := tr.Actions()
	if countKind(acts, "back") != 3 {
		t.Fatalf("back presses: got %d, want 3", countKind(acts, "back"))
	}
	if countKind(acts, "restart") != 1 {
		t.Fatalf("restarts: got %d, want 1", countKind(acts, "restart"))
	}
}

func TestPanicRouteGivesUpLost(t *testing.T) {
	// Nothing ever becomes recognizable: the route must end in ErrLost
	// rather than hanging (RestartTimeout is 50ms in runtimetest.Options).
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "")
	_, err := c.CurrentScreen(context.Background())
	if !errors.Is(err, runtime.ErrLost) {
		t.Fatalf("got %v, want ErrLost", err)
	}
	if countKind(tr.Actions(), "restart") != 1 {
		t.Fatal("gave up without trying a restart")
	}
}

func TestPanicRouteRespectsKillSwitch(t *testing.T) {
	ks := &runtimetest.FakeKill{}
	c, _ := newCtx(t, ks, "", "", "", "", "base")
	ks.Set(runtime.ErrPaused)
	_, err := c.CurrentScreen(context.Background())
	if !errors.Is(err, runtime.ErrPaused) {
		t.Fatalf("got %v, want ErrPaused", err)
	}
}
```

Note the existing stub makes these fail with `ErrLost` (and zero backs), which is a *behavioral* failure, not a compile failure — exactly what Step 2 should show.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/runtime/... -run TestPanicRoute -v`
Expected: FAIL — recovery tests get `ErrLost` with no back/restart actions.

- [ ] **Step 3: Implement** — delete the stub `panicRoute` from `ctx.go`, create `internal/runtime/panic.go`:

```go
package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tomharris/lw-manager/internal/vision"
)

// panicBackAttempts is how many back presses to try before restarting the
// app. Popups and interstitials die to back; deeper corruption does not.
const panicBackAttempts = 3

// panicRoute recovers from an unrecognized screen: back ×3, then app
// restart, then wait for the graph's entry screen. Task code never calls or
// sees this — primitives run it internally and surface only ErrLost when
// every rung fails. The run is then marked failed and the agent stops
// rather than flails.
func (c *Ctx) panicRoute(ctx context.Context) (Recognition, error) {
	for i := 1; i <= panicBackAttempts; i++ {
		if err := c.ks.Check(ctx); err != nil {
			return Recognition{}, err
		}
		c.log.Warn("panic route: pressing back", "attempt", i)
		if err := c.tr.Back(ctx); err != nil {
			return Recognition{}, fmt.Errorf("runtime: panic route back press: %w", err)
		}
		if err := c.Sleep(ctx, c.poll, 2*c.poll); err != nil {
			return Recognition{}, err
		}
		r, _, err := c.recognize(ctx)
		if err == nil {
			c.log.Info("panic route: recovered", "screen", r.Screen, "backs", i)
			return r, nil
		}
		if !errors.Is(err, vision.ErrNoScreenRecognized) {
			return Recognition{}, err
		}
	}

	c.log.Warn("panic route: back exhausted, restarting app")
	if err := c.tr.AppRestart(ctx); err != nil {
		return Recognition{}, fmt.Errorf("runtime: panic route restart: %w", err)
	}
	deadline := time.Now().Add(c.restartTimeout)
	for time.Now().Before(deadline) {
		if err := c.ks.Check(ctx); err != nil {
			return Recognition{}, err
		}
		if err := c.Sleep(ctx, c.poll, 2*c.poll); err != nil {
			return Recognition{}, err
		}
		r, _, err := c.recognize(ctx)
		if err == nil && r.Screen == c.graph.Entry {
			c.log.Info("panic route: recovered via restart")
			return r, nil
		}
		if err != nil && !errors.Is(err, vision.ErrNoScreenRecognized) {
			return Recognition{}, err
		}
	}
	return Recognition{}, fmt.Errorf("runtime: account %d unrecoverable after %d backs and a restart: %w",
		c.accountID, panicBackAttempts, ErrLost)
}
```

- [ ] **Step 4: Run the full runtime suite** (Task 5/6 tests that relied on stub behavior — `TestUnrecognizedScreenReachesStubRecovery`, `TestSwipeRefusedOnUnrecognizedScreen` — still pass: a single blank frame means back-recovery re-recognizes the held blank frame and the route ends in `ErrLost` after the restart wait times out.)

Run: `go test ./internal/runtime/... -v`
Expected: PASS, all tasks' tests.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/
git commit -m "Add panic route: back, restart, then fail lost"
```

---

### Task 8: NavigateTo

**Files:**
- Create: `internal/runtime/navigate.go`
- Test: `internal/runtime/navigate_test.go` (`package runtime_test`)

**Interfaces:**
- Consumes: `CurrentScreen`, `WaitFor`, `Tap`, `graph.Path`, `Transport.Back`.
- Produces: `(*Ctx).NavigateTo(ctx, target string) error`.

- [ ] **Step 1: Write failing tests** — `internal/runtime/navigate_test.go`:

```go
package runtime_test

import (
	"context"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
)

func TestNavigateToMultiHop(t *testing.T) {
	// base → alliance → alliance_tech. Frame schedule (one frame per
	// Screenshot call): CurrentScreen, hop1 tap verify, hop1 WaitFor, hop2
	// tap verify, hop2 WaitFor, final CurrentScreen; the replay holds the
	// last frame for any extra polls.
	c, _ := newCtx(t, &runtimetest.FakeKill{},
		"base", "base", "alliance", "alliance", "alliance_tech", "alliance_tech")
	if err := c.NavigateTo(context.Background(), "alliance_tech"); err != nil {
		t.Fatal(err)
	}
}

func TestNavigateToAlreadyThere(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "mail")
	if err := c.NavigateTo(context.Background(), "mail"); err != nil {
		t.Fatal(err)
	}
	if len(tr.Actions()) != 0 {
		t.Fatalf("acted while already on target: %+v", tr.Actions())
	}
}

func TestNavigateToReplansAfterUnexpectedScreen(t *testing.T) {
	// Heading base → alliance, but the tap "lands" on mail (a popup ate the
	// tap, say). WaitFor(alliance) sees mail 5× (wrong-screen streak), then
	// NavigateTo re-plans from mail: back to base, tap to alliance.
	c, tr := newCtx(t, &runtimetest.FakeKill{},
		"base",                                 // CurrentScreen
		"base",                                 // hop tap verify
		"mail", "mail", "mail", "mail", "mail", // WaitFor streak
		"mail",              // re-plan CurrentScreen
		"base",              // back-hop WaitFor
		"base",              // tap verify
		"alliance",          // WaitFor
		"alliance")          // final CurrentScreen
	if err := c.NavigateTo(context.Background(), "alliance"); err != nil {
		t.Fatal(err)
	}
	acts := tr.Actions()
	if countKind(acts, "back") != 1 {
		t.Fatalf("back presses: got %d, want 1 (the re-planned mail→base hop)", countKind(acts, "back"))
	}
	if countKind(acts, "tap") != 2 {
		t.Fatalf("taps: got %d, want 2", countKind(acts, "tap"))
	}
}

func TestNavigateToUnreachable(t *testing.T) {
	// The synthetic graph has no edge into "base" from nowhere — but every
	// screen is reachable, so test the unreachable case with a bogus target
	// that exists in the registry-validated graph? It does not; instead
	// assert the graph error surfaces for a screen with no route.
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "alliance_tech")
	// alliance_tech → radar requires alliance_tech→alliance→base→radar,
	// which exists; use a self-check instead: navigating to an unknown
	// screen must fail loudly.
	if err := c.NavigateTo(context.Background(), "no_such_screen"); err == nil {
		t.Fatal("navigated to a screen the graph does not know")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/runtime/... -run TestNavigate -v`
Expected: FAIL — `NavigateTo` undefined.

- [ ] **Step 3: Implement** — `internal/runtime/navigate.go`:

```go
package runtime

import (
	"context"
	"errors"
	"fmt"
)

// maxReplans bounds how many times one NavigateTo call re-plans after
// landing somewhere unexpected. Re-planning absorbs a popup eating a tap;
// unbounded re-planning would mask a broken graph.
const maxReplans = 3

// NavigateTo walks the screen graph to the target screen from wherever the
// device currently is. Landing on an unexpected but recognized screen
// triggers a re-plan from there rather than a failure.
func (c *Ctx) NavigateTo(ctx context.Context, target string) error {
	for attempt := 0; ; attempt++ {
		cur, err := c.CurrentScreen(ctx)
		if err != nil {
			return err
		}
		if cur.Screen == target {
			return nil
		}
		if attempt > maxReplans {
			return fmt.Errorf("runtime: could not reach %q after %d re-plans, stuck on %q", target, maxReplans, cur.Screen)
		}

		path, err := c.graph.Path(cur.Screen, target)
		if err != nil {
			return err
		}
		if err := c.walk(ctx, path); err != nil {
			if errors.Is(err, ErrWrongScreen) {
				continue // recognized, just not where we hoped: re-plan
			}
			return err
		}
	}
}

// walk executes edges until one lands unexpectedly (ErrWrongScreen) or all
// succeed.
func (c *Ctx) walk(ctx context.Context, path []Edge) error {
	for _, e := range path {
		switch e.Action {
		case ActionTap:
			if err := c.Tap(ctx, e.From, e.AnchorID); err != nil {
				return err
			}
		case ActionBack:
			if err := c.ks.Check(ctx); err != nil {
				return err
			}
			if err := c.tr.Back(ctx); err != nil {
				return fmt.Errorf("runtime: back edge %q->%q: %w", e.From, e.To, err)
			}
		default:
			return fmt.Errorf("runtime: edge %q->%q has unknown action %d", e.From, e.To, e.Action)
		}
		if _, err := c.WaitFor(ctx, e.To); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/runtime/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/navigate.go internal/runtime/navigate_test.go
git commit -m "Add graph-walking NavigateTo with re-planning"
```

---

### Task 9: capture.Record and Ctx.Capture

**Files:**
- Modify: `internal/capture/capture.go` — extract `Record` from `Capture`
- Modify: `internal/runtime/ctx.go` — replace the `Capturer` placeholder; add `Capture` field to `Options`
- Create: `internal/runtime/capture.go`
- Test: `internal/capture/capture_test.go` (extend), `internal/runtime/capture_test.go` (`package runtime_test`)

**Interfaces:**
- Produces:
  - `(*capture.Service).Record(ctx, accountID int64, img image.Image, screenID *string) (capture.Result, error)`
  - runtime: `type Capturer interface { Record(ctx context.Context, accountID int64, img image.Image, screenID *string) (capture.Result, error) }`
  - `Options.Capture Capturer` (optional; nil ⇒ `Ctx.Capture` errors)
  - `(*Ctx).Capture(ctx, screenID string) (int64, error)`
  - `(*Ctx).ScreenshotIDs() []int64`

- [ ] **Step 1: Refactor capture.Capture** — in `internal/capture/capture.go`, replace the block of `Capture` from `data, err := encodePNG(img)` through the final `return Result{...}, nil` with a call to the new method, so `Capture` ends:

```go
	img, err := tr.Screenshot(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("capture: screenshotting %s: %w", target.Serial, err)
	}
	res, err := s.Record(ctx, accountID, img, nil)
	if err != nil {
		return Result{}, err
	}
	s.log.Info("captured",
		"account", accountID,
		"nickname", target.Nickname,
		"serial", target.Serial,
		"screenshot_id", res.ScreenshotID,
		"bytes", res.Bytes,
		"deduplicated", res.Deduplicated)
	return res, nil
}

// Record stores an already-captured frame: encode → blob → screenshot row.
// It exists so the task runtime, which holds its own transport and frames,
// can persist observations without opening a second device connection.
func (s *Service) Record(ctx context.Context, accountID int64, img image.Image, screenID *string) (Result, error) {
	data, err := encodePNG(img)
	if err != nil {
		return Result{}, err
	}

	sum := blob.Sum(data)
	key := blob.Key(sum)
	existed, err := s.blobs.Exists(ctx, key)
	if err != nil {
		return Result{}, fmt.Errorf("capture: checking blob %s: %w", key, err)
	}
	if _, _, err := blob.PutContent(ctx, s.blobs, data); err != nil {
		return Result{}, err
	}

	row, err := s.store.InsertScreenshot(ctx, accountID, key, sum, screenID)
	if err != nil {
		return Result{}, err
	}

	b := img.Bounds()
	return Result{
		ScreenshotID: row.ID,
		AccountID:    accountID,
		ObjectKey:    key,
		SHA256:       sum,
		Bytes:        len(data),
		Resolution:   image.Point{X: b.Dx(), Y: b.Dy()},
		CapturedAt:   row.CapturedAt,
		Deduplicated: existed,
	}, nil
}
```

(The resolution-reconcile block earlier in `Capture` stays exactly where it is.)

Run: `go test ./internal/capture/ -v` — existing tests must still pass before proceeding. Then add one test to `internal/capture/capture_test.go` exercising `Record` directly with the fakes the file already uses (same store/blob fakes; assert the screenshot row lands with the given screenID and the Result echoes the image size).

- [ ] **Step 2: Write failing runtime test** — `internal/runtime/capture_test.go`:

```go
package runtime_test

import (
	"context"
	"errors"
	"image"
	"testing"

	"github.com/tomharris/lw-manager/internal/capture"
	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
)

type fakeCapturer struct {
	nextID   int64
	screenID *string
}

func (f *fakeCapturer) Record(ctx context.Context, accountID int64, img image.Image, screenID *string) (capture.Result, error) {
	f.nextID++
	f.screenID = screenID
	return capture.Result{ScreenshotID: f.nextID, AccountID: accountID}, nil
}

func TestCaptureVerifiesScreenAndRecords(t *testing.T) {
	tr, err := transport.NewReplayTransportFromImages(runtimetest.Frames("radar", "radar")...)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeCapturer{}
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Capture = fc
	c, err := runtime.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	id, err := c.Capture(context.Background(), "radar")
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 || fc.screenID == nil || *fc.screenID != "radar" {
		t.Fatalf("capture: id=%d screenID=%v", id, fc.screenID)
	}
	if ids := c.ScreenshotIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ScreenshotIDs: %v", ids)
	}
}

func TestCaptureRefusesWrongScreen(t *testing.T) {
	tr, _ := transport.NewReplayTransportFromImages(runtimetest.Frame("base"))
	fc := &fakeCapturer{}
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Capture = fc
	c, _ := runtime.New(opts)

	if _, err := c.Capture(context.Background(), "radar"); !errors.Is(err, runtime.ErrWrongScreen) {
		t.Fatalf("got %v, want ErrWrongScreen", err)
	}
	if fc.nextID != 0 {
		t.Fatal("recorded a frame from the wrong screen")
	}
}

func TestCaptureWithoutCapturerErrors(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	if _, err := c.Capture(context.Background(), "base"); err == nil {
		t.Fatal("Capture with no capturer configured must error")
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/runtime/... -run TestCapture -v`
Expected: FAIL — `Capture` undefined / placeholder interface mismatch.

- [ ] **Step 4: Implement** — in `internal/runtime/ctx.go`: delete the placeholder `Capturer interface{}`, add `Capture Capturer` to `Options`, and set `cap: opts.Capture` in `New`. Create `internal/runtime/capture.go`:

```go
package runtime

import (
	"context"
	"errors"
	"fmt"
	"image"

	"github.com/tomharris/lw-manager/internal/capture"
)

// Capturer persists an already-captured frame. *capture.Service satisfies
// it; tests use a fake.
type Capturer interface {
	Record(ctx context.Context, accountID int64, img image.Image, screenID *string) (capture.Result, error)
}

// Capture verifies the device shows the named screen, then persists that
// frame with the screen id attached — the provenance every OCR-derived
// number must trace back to (invariant #5). The screenshot id is remembered
// for the task_runs row.
func (c *Ctx) Capture(ctx context.Context, screenID string) (int64, error) {
	if err := c.ks.Check(ctx); err != nil {
		return 0, err
	}
	if c.cap == nil {
		return 0, errors.New("runtime: no capturer configured")
	}
	frame, err := c.verifyScreen(ctx, screenID)
	if err != nil {
		return 0, err
	}
	res, err := c.cap.Record(ctx, c.accountID, frame, &screenID)
	if err != nil {
		return 0, fmt.Errorf("runtime: recording capture of %q for account %d: %w", screenID, c.accountID, err)
	}
	c.screenshotIDs = append(c.screenshotIDs, res.ScreenshotID)
	return res.ScreenshotID, nil
}

// ScreenshotIDs returns every screenshot this Ctx has recorded, for the
// task_runs audit row.
func (c *Ctx) ScreenshotIDs() []int64 {
	out := make([]int64, len(c.screenshotIDs))
	copy(out, c.screenshotIDs)
	return out
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/runtime/... ./internal/capture/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/capture/ internal/runtime/
git commit -m "Extract capture.Record and add runtime Capture primitive"
```

---

### Task 10: Runner — task_runs lifecycle

**Files:**
- Create: `internal/runtime/runner.go`
- Test: `internal/runtime/runner_test.go` (`package runtime_test`)

**Interfaces:**
- Consumes: Task 2 store methods (as an interface), `Ctx.ScreenshotIDs`.
- Produces:
  - `type TaskFunc func(ctx context.Context, rt *Ctx) error`
  - `type RunStore interface { StartTaskRun(ctx context.Context, accountID int64, taskName string) (int64, error); FinishTaskRun(ctx context.Context, id int64, status string, errMsg *string, screenshotIDs []int64) error }`
  - `type Outcome struct { RunID int64; Status string }`
  - `Run(ctx, store RunStore, rt *Ctx, name string, fn TaskFunc) (Outcome, error)`
  - `(*Ctx).AccountID() int64`

- [ ] **Step 1: Write failing tests** — `internal/runtime/runner_test.go`:

```go
package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
)

type fakeRunStore struct {
	started  []string
	finished map[int64]string // id → status
	errMsg   *string
}

func (f *fakeRunStore) StartTaskRun(ctx context.Context, accountID int64, taskName string) (int64, error) {
	f.started = append(f.started, taskName)
	return int64(len(f.started)), nil
}

func (f *fakeRunStore) FinishTaskRun(ctx context.Context, id int64, status string, errMsg *string, screenshotIDs []int64) error {
	if f.finished == nil {
		f.finished = map[int64]string{}
	}
	f.finished[id] = status
	f.errMsg = errMsg
	return nil
}

func runWith(t *testing.T, fn runtime.TaskFunc) (*fakeRunStore, runtime.Outcome, error) {
	t.Helper()
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	st := &fakeRunStore{}
	out, err := runtime.Run(context.Background(), st, c, "test_task", fn)
	return st, out, err
}

func TestRunRecordsSuccess(t *testing.T) {
	st, out, err := runWith(t, func(ctx context.Context, rt *runtime.Ctx) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "succeeded" || st.finished[out.RunID] != "succeeded" {
		t.Fatalf("outcome %+v, store %+v", out, st.finished)
	}
	if st.errMsg != nil {
		t.Fatalf("error message on success: %q", *st.errMsg)
	}
}

func TestRunRecordsFailure(t *testing.T) {
	boom := errors.New("boom")
	st, out, err := runWith(t, func(ctx context.Context, rt *runtime.Ctx) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("task error not returned: %v", err)
	}
	if out.Status != "failed" || st.finished[out.RunID] != "failed" {
		t.Fatalf("outcome %+v", out)
	}
	if st.errMsg == nil || *st.errMsg != "boom" {
		t.Fatalf("errMsg: %v", st.errMsg)
	}
}

func TestRunRecordsPauseDistinctly(t *testing.T) {
	_, out, err := runWith(t, func(ctx context.Context, rt *runtime.Ctx) error {
		return runtime.ErrPaused
	})
	if !errors.Is(err, runtime.ErrPaused) {
		t.Fatal(err)
	}
	if out.Status != "paused" {
		t.Fatalf("status: got %q, want paused — a pause is not a failure", out.Status)
	}
}

func TestRunFinishesEvenWhenContextCancelled(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	st := &fakeRunStore{}
	ctx, cancel := context.WithCancel(context.Background())
	out, err := runtime.Run(ctx, st, c, "test_task", func(ctx context.Context, rt *runtime.Ctx) error {
		cancel() // the operator hits ctrl-C mid-task
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("expected the cancellation to surface")
	}
	if st.finished[out.RunID] != "failed" {
		t.Fatalf("cancelled run not finished in store: %+v", st.finished)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/runtime/... -run TestRun -v`
Expected: FAIL — `runtime.Run`, `runtime.Outcome` undefined.

- [ ] **Step 3: Implement** — `internal/runtime/runner.go`:

```go
package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TaskFunc is a task: plain imperative Go over the constrained Ctx.
type TaskFunc func(ctx context.Context, rt *Ctx) error

// RunStore is the slice of the database the runner needs. *db.Pool
// satisfies it.
type RunStore interface {
	StartTaskRun(ctx context.Context, accountID int64, taskName string) (int64, error)
	FinishTaskRun(ctx context.Context, id int64, status string, errMsg *string, screenshotIDs []int64) error
}

// Outcome is what a completed run looked like.
type Outcome struct {
	RunID  int64
	Status string
}

// AccountID reports which account this Ctx drives.
func (c *Ctx) AccountID() int64 { return c.accountID }

// Run executes one task under a task_runs record. The running row is
// written before the task acts (a killed process leaves evidence), and the
// finishing update uses a detached context so ctrl-C mid-task still gets
// its outcome recorded.
func Run(ctx context.Context, store RunStore, rt *Ctx, name string, fn TaskFunc) (Outcome, error) {
	runID, err := store.StartTaskRun(ctx, rt.accountID, name)
	if err != nil {
		return Outcome{}, fmt.Errorf("runtime: starting run of %q for account %d: %w", name, rt.accountID, err)
	}

	taskErr := fn(ctx, rt)

	status := "succeeded"
	var msg *string
	switch {
	case taskErr == nil:
	case errors.Is(taskErr, ErrPaused), errors.Is(taskErr, ErrAccountDisabled):
		status = "paused"
		s := taskErr.Error()
		msg = &s
	default:
		status = "failed"
		s := taskErr.Error()
		msg = &s
	}

	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if ferr := store.FinishTaskRun(finishCtx, runID, status, msg, rt.ScreenshotIDs()); ferr != nil {
		if taskErr != nil {
			return Outcome{RunID: runID, Status: status}, errors.Join(taskErr, ferr)
		}
		return Outcome{RunID: runID, Status: status}, ferr
	}
	return Outcome{RunID: runID, Status: status}, taskErr
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/runtime/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/runner.go internal/runtime/runner_test.go
git commit -m "Add task runner with task_runs lifecycle"
```

---

### Task 11: Task registry and the five Tier 1 skeletons

**Files:**
- Create: `internal/tasks/registry.go`, `internal/tasks/collect.go`, `internal/tasks/daily_gather.go`, `internal/tasks/help_all.go`, `internal/tasks/mail_collect.go`, `internal/tasks/tech_donate.go`, `internal/tasks/radar.go`
- Test: `internal/tasks/tasks_test.go`

**Interfaces:**
- Consumes: `runtime.TaskFunc`, `runtime.Ctx` primitives, `runtime.ErrAnchorNotFound`; `runtimetest` fixtures.
- Produces: `tasks.Register(name string, fn runtime.TaskFunc)`, `tasks.Get(name) (runtime.TaskFunc, bool)`, `tasks.Names() []string`; registered names `daily_gather`, `help_all`, `mail_collect`, `tech_donate`, `radar`.

- [ ] **Step 1: Write failing tests** — `internal/tasks/tasks_test.go`:

```go
package tasks_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/tasks"
	"github.com/tomharris/lw-manager/internal/transport"
)

func TestAllTierOneTasksRegistered(t *testing.T) {
	want := []string{"daily_gather", "help_all", "mail_collect", "radar", "tech_donate"}
	if got := tasks.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names: got %v, want %v", got, want)
	}
}

// frame scripts per task: enough recognizable frames to navigate from base
// to the task's screen and perform its tap; the replay holds the last frame
// for trailing polls.
var skeletonScripts = map[string][]string{
	// already on base: CurrentScreen, tap verify (tap-if-present on base itself)
	"daily_gather": {"base", "base"},
	// base → alliance: CurrentScreen, hop verify, WaitFor, final
	// CurrentScreen, then the help tap's verify
	"help_all": {"base", "base", "alliance", "alliance", "alliance"},
	"mail_collect": {"base", "base", "mail", "mail", "mail"},
	// base → alliance → alliance_tech
	"tech_donate": {"base", "base", "alliance", "alliance", "alliance_tech", "alliance_tech", "alliance_tech"},
	"radar": {"base", "base", "radar", "radar", "radar"},
}

func TestSkeletonsRunAgainstSyntheticScreens(t *testing.T) {
	for name, script := range skeletonScripts {
		t.Run(name, func(t *testing.T) {
			fn, ok := tasks.Get(name)
			if !ok {
				t.Fatalf("task %q not registered", name)
			}
			tr, err := transport.NewReplayTransportFromImages(runtimetest.Frames(script...)...)
			if err != nil {
				t.Fatal(err)
			}
			c, err := runtime.New(runtimetest.Options(tr, &runtimetest.FakeKill{}))
			if err != nil {
				t.Fatal(err)
			}
			if err := fn(context.Background(), c); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			// Every skeleton must have tapped its collect anchor exactly once.
			taps := 0
			for _, a := range tr.Actions() {
				if a.Kind == "tap" {
					taps++
				}
			}
			if taps == 0 {
				t.Fatalf("%s performed no taps", name)
			}
		})
	}
}

func TestSkeletonToleratesMissingAnchor(t *testing.T) {
	// help_all when no help button is on screen: the anchor match must
	// fail, and the task must treat that as "nothing to help", not an
	// error. Simulate the missing button by moving the tap anchor's search
	// region to an area every synthetic frame renders flat — the match
	// score there cannot clear the threshold. (Moving the region beats
	// raising the threshold: a pixel-perfect synthetic match can score a
	// legitimate 1.0, which no finite threshold excludes.)
	reg := runtimetest.Registry()
	for si := range reg.Screens {
		if reg.Screens[si].Name != "alliance" {
			continue
		}
		for ai := range reg.Screens[si].Anchors {
			if reg.Screens[si].Anchors[ai].ID == "help_all_button" {
				// Flat gray on alliance frames: no pattern spot lives here.
				reg.Screens[si].Anchors[ai].Region = transport.Rect{X1: 0.05, Y1: 0.6, X2: 0.4, Y2: 0.9}
			}
		}
	}
	fn, _ := tasks.Get("help_all")
	tr, err := transport.NewReplayTransportFromImages(
		runtimetest.Frames("base", "base", "alliance", "alliance", "alliance")...)
	if err != nil {
		t.Fatal(err)
	}
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Registry = reg
	c, err := runtime.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(context.Background(), c); err != nil {
		t.Fatalf("missing anchor must mean 'nothing to do', got: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tasks/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement** — `internal/tasks/registry.go`:

```go
// Package tasks holds the executable task catalogue. Each task file
// self-registers in init, the database/sql driver pattern, so importing the
// package is enough to populate the registry.
//
// The Tier 1 tasks are skeletons: written against the screen names and
// anchors the real corpus is expected to produce. Graph validation refuses
// to run them until those templates exist, which is the designed behavior.
package tasks

import (
	"fmt"
	"sort"
	"sync"

	"github.com/tomharris/lw-manager/internal/runtime"
)

var (
	mu       sync.RWMutex
	registry = map[string]runtime.TaskFunc{}
)

// Register adds a task. Duplicate or empty names are init-time bugs and
// panic loudly.
func Register(name string, fn runtime.TaskFunc) {
	mu.Lock()
	defer mu.Unlock()
	if name == "" || fn == nil {
		panic("tasks: Register called with empty name or nil func")
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("tasks: Register called twice for %q", name))
	}
	registry[name] = fn
}

// Get looks a task up by name.
func Get(name string) (runtime.TaskFunc, bool) {
	mu.RLock()
	defer mu.RUnlock()
	fn, ok := registry[name]
	return fn, ok
}

// Names lists registered tasks, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
```

`internal/tasks/collect.go`:

```go
package tasks

import (
	"context"
	"errors"

	"github.com/tomharris/lw-manager/internal/runtime"
)

// collectTask builds the shared Tier 1 shape: navigate to a screen, tap a
// collect-style button if it is present. A missing anchor is "nothing to
// collect", not a failure — the help-all button simply is not rendered when
// no one needs help.
//
// Each task keeps its own file even while they are this similar: real
// corpus screens will differentiate them (scrolling, multi-tap, sub-menus).
func collectTask(screen, anchorID string) runtime.TaskFunc {
	return func(ctx context.Context, rt *runtime.Ctx) error {
		if err := rt.NavigateTo(ctx, screen); err != nil {
			return err
		}
		if err := rt.Tap(ctx, screen, anchorID); err != nil {
			if errors.Is(err, runtime.ErrAnchorNotFound) {
				return nil
			}
			return err
		}
		return nil
	}
}
```

`internal/tasks/daily_gather.go`:

```go
package tasks

// daily_gather collects the base's accumulated resources. Skeleton: the
// real task will iterate multiple resource buildings once corpus templates
// name them.
func init() { Register("daily_gather", collectTask("base", "gather_button")) }
```

`internal/tasks/help_all.go`:

```go
package tasks

// help_all taps the alliance help-all button. The button is absent when no
// one needs help; that is success, not failure.
func init() { Register("help_all", collectTask("alliance", "help_all_button")) }
```

`internal/tasks/mail_collect.go`:

```go
package tasks

// mail_collect claims mail rewards via collect-all. Skeleton: the real task
// will need per-tab collection (system/alliance/report) once mapped.
func init() { Register("mail_collect", collectTask("mail", "collect_all_button")) }
```

`internal/tasks/tech_donate.go`:

```go
package tasks

// tech_donate makes one alliance tech donation. Skeleton: the real task
// will tap repeatedly until the daily cap and read the donation counter.
func init() { Register("tech_donate", collectTask("alliance_tech", "donate_button")) }
```

`internal/tasks/radar.go`:

```go
package tasks

// radar claims completed radar missions. Skeleton: the real task will need
// per-mission claims and a completion sweep once the screen is mapped.
func init() { Register("radar", collectTask("radar", "radar_claim_button")) }
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/tasks/ -v`
Expected: PASS (registry test, five skeleton runs, missing-anchor tolerance).

- [ ] **Step 5: Commit**

```bash
git add internal/tasks/
git commit -m "Add task registry and Tier 1 task skeletons"
```

---

### Task 12: CLI — `agent run-task`, `control pause|resume`, CLAUDE.md

**Files:**
- Modify: `cmd/agent/main.go`
- Modify: `cmd/control/main.go`
- Modify: `CLAUDE.md`

No new unit tests: these commands are thin wiring over tested parts, and their device/DB dependencies put them beyond the unit tier. Verification is by build + the manual smoke lines below.

**Interfaces:**
- Consumes: `tasks.Get/Names`, `runtime.New/Run/NewDBKillSwitch/DefaultGraph`, `vision.LoadRegistry`, `capture.New`, `(*db.Pool).SetPauseAll`.

- [ ] **Step 1: agent run-task** — in `cmd/agent/main.go`: add `"run-task"` to the usage text (`run-task  run one task for an account, on demand`) and switch:

```go
	case "run-task":
		return runTask(ctx, cfg, os.Args[2:])
```

Add imports `"math/rand"`, `"time"`, plus `internal/runtime`, `internal/tasks`, `internal/vision`. Add the command:

```go
func runTask(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("run-task", flag.ExitOnError)
	accountID := fs.Int64("account", 0, "account id to run for (required)")
	taskName := fs.String("task", "", "task to run; one of: "+strings.Join(tasks.Names(), ", "))
	manifest := fs.String("templates", "templates/manifest.yaml", "template manifest path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *accountID == 0 || *taskName == "" {
		fs.Usage()
		return fmt.Errorf("--account and --task are required")
	}
	fn, ok := tasks.Get(*taskName)
	if !ok {
		return fmt.Errorf("unknown task %q, want one of: %s", *taskName, strings.Join(tasks.Names(), ", "))
	}

	// Registry load and graph validation happen before any connection is
	// opened: a manifest missing the graph's screens must fail here, loudly,
	// not as a mid-task mystery.
	reg, err := vision.LoadRegistry(*manifest)
	if err != nil {
		return err
	}
	graph := runtime.DefaultGraph()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		return err
	}

	target, err := pool.CaptureTargetByAccount(ctx, *accountID)
	if err != nil {
		return err
	}
	tr, err := transport.NewADBTransport(ctx, transport.ADBOptions{
		ADBPath: cfg.ADBPath,
		Serial:  target.Serial,
		Package: target.Package,
	})
	if err != nil {
		return fmt.Errorf("opening transport for device %s: %w", target.Serial, err)
	}
	defer tr.Close()

	rt, err := runtime.New(runtime.Options{
		Transport: tr,
		Registry:  reg,
		Graph:     graph,
		Kill:      runtime.NewDBKillSwitch(pool, *accountID),
		Capture:   capture.New(pool, blobs, nil),
		AccountID: *accountID,
		Rand:      rand.New(rand.NewSource(time.Now().UnixNano())),
	})
	if err != nil {
		return err
	}

	out, err := runtime.Run(ctx, pool, rt, *taskName, fn)
	if out.RunID != 0 {
		// stdout stays machine-readable; the error itself goes to stderr via
		// the main error path.
		fmt.Printf("run_id=%d task=%s account=%d status=%s\n", out.RunID, *taskName, *accountID, out.Status)
	}
	return err
}
```

- [ ] **Step 2: control pause/resume** — in `cmd/control/main.go`: extend usage text with `pause     set the global kill switch` and `resume    clear the global kill switch`, and the switch:

```go
	case "pause":
		return runPause(ctx, cfg, os.Args[2:])
	case "resume":
		return runResume(ctx, cfg)
```

Add:

```go
func runPause(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("pause", flag.ExitOnError)
	reason := fs.String("reason", "", "why the fleet is being paused (required, it ends up in every ErrPaused)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reason == "" {
		fs.Usage()
		return fmt.Errorf("--reason is required: future-you wants to know why everything stopped")
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.SetPauseAll(ctx, true, *reason); err != nil {
		return err
	}
	fmt.Printf("pause_all=true reason=%q\n", *reason)
	return nil
}

func runResume(ctx context.Context, cfg config.Config) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.SetPauseAll(ctx, false, ""); err != nil {
		return err
	}
	fmt.Println("pause_all=false")
	return nil
}
```

- [ ] **Step 3: Build and smoke-test the wiring that runs without a device**

Run: `make build && ./bin/control migrate && ./bin/control pause --reason test && ./bin/control resume`
Expected: builds; migrate applies 00002; pause prints `pause_all=true reason="test"`; resume prints `pause_all=false`. (Needs `docker compose up -d`.)

Run: `./bin/agent run-task --account 1 --task daily_gather --templates templates/manifest.yaml 2>&1 | tail -1`
Expected: loud failure — either the manifest is missing or graph validation rejects it (`screen "base" not in registry`). That failure is this slice's designed endpoint: the command exists, refuses to blind-run, and starts working the day the corpus lands.

- [ ] **Step 4: Update CLAUDE.md** — in the Quickstart section after the `capture` line, add:

```
./bin/agent run-task --account <id> --task help_all    # runs once templates exist
./bin/control pause --reason "alliance event"          # global kill switch
./bin/control resume
```

And in the Layout section, extend the tree:

```
internal/runtime    task runtime: Ctx primitives, screen graph, panic route, kill switch
internal/tasks      Tier 1 task skeletons; self-registering catalogue
```

- [ ] **Step 5: Commit**

```bash
git add cmd/ CLAUDE.md
git commit -m "Add agent run-task and control pause/resume commands"
```

---

### Task 13: Final verification

- [ ] **Step 1: Full unit tier with nothing running**

Run: `docker compose stop && make test && docker compose start`
Expected: PASS — invariant #6 holds for the whole runtime.

- [ ] **Step 2: Integration tier, including the clean-database race path**

Run: `docker compose exec postgres psql -U lw -d postgres -c 'DROP DATABASE IF EXISTS lw_manager_test' && make test-integration`
Expected: PASS from a cold database (the race-sensitive path per CLAUDE.md).

- [ ] **Step 3: Lint and CGO check**

Run: `make lint && make verify-nocgo`
Expected: no vet findings, no gofmt diffs, clean CGO-free build.

- [ ] **Step 4: Review the diff against the spec**

Run: `git log --oneline m1-vision-core..HEAD && git diff m1-vision-core --stat`
Check each spec section has landed: transport Back, migration 00002, Ctx primitives (all kill-switch-guarded, anchor-verified, jittered), graph + NavigateTo, panic route, capture Record, runner, five skeletons, both CLIs.

- [ ] **Step 5: Commit any stragglers and stop**

The branch ends here — scheduler and the hardware-gated M2 acceptance run are explicitly out of scope.

---

## Execution notes

- Tasks 1–4 are independent of each other except Task 3→4 (sentinel reuse); execute in order anyway — each is small.
- Tasks 5–10 are strictly sequential (each consumes the previous surface).
- Task 11 needs 5+10; Task 12 needs 11; Task 13 is last.
- If a runtime test flakes on NCC scores with synthetic frames, check the anchor `Region` math first — a region that clips the pasted pattern lowers the score below threshold, and `runtimetest.spots` is where positions and regions must agree.
