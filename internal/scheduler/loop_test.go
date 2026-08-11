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

// accountOutsideWindow finds an account whose derived offline window does not
// cover now, so a test about the weekday gate is not silently decided by the
// window instead.
func accountOutsideWindow(t *testing.T, now time.Time) int64 {
	t.Helper()
	for id := int64(1); id < 500; id++ {
		if !inOfflineWindow(id, now) {
			return id
		}
	}
	t.Fatal("no account outside the offline window at this instant")
	return 0
}

// Plan documents that now arrives "already in the operator's location", and
// New is the only place that decides what that location is. Defaulting to UTC
// silently satisfies the type and violates the contract, which is a defect no
// test of Plan itself can catch — Plan is handed whatever it is handed.
//
// The M2 24-hour run is what this costs: the offline window landed 19:37-01:55
// local, taking the device off through the operator's evening and running it
// at 2-6am. For a detection-avoidance feature that is close to backwards.
func TestNewDefaultsToTheOperatorLocationNotUTC(t *testing.T) {
	l, err := New(Options{Store: &fakeStore{snaps: []Snapshot{dueSnapshot()}}, Executor: &recordExec{}})
	if err != nil {
		t.Fatal(err)
	}
	if l.loc != time.Local {
		t.Errorf("default location is %v, want time.Local — Plan's contract is the operator's day", l.loc)
	}
}

// The weekday gate reads now.Weekday(), so the location now carries decides
// which day it is. UTC and the operator's day diverge through the last hours
// of every local evening — precisely when an evening task is scheduled.
//
// This is the second observed cost of the UTC default: radar is gated to
// {1,3,5,6}, and in UTC its Monday ended at 20:00 EDT, so the run expected
// near 22:07 EDT never fired.
func TestTickOnceEvaluatesTheWeekdayGateInTheConfiguredLocation(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata on this host: %v", err)
	}
	// 01:00 Tuesday UTC is 21:00 Monday in New York.
	instant := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	if got := instant.In(ny).Weekday(); got != time.Monday {
		t.Fatalf("fixture drifted: local weekday is %v, want Monday", got)
	}
	if got := instant.UTC().Weekday(); got != time.Tuesday {
		t.Fatalf("fixture drifted: UTC weekday is %v, want Tuesday", got)
	}

	acct := accountOutsideWindow(t, instant.In(ny))
	snap := Snapshot{
		Accounts: []Account{{ID: acct, Role: "alliance_data", Enabled: true}},
		Tasks: []Task{{
			Name:    "radar",
			Cadence: time.Hour,
			Roles:   []string{"alliance_data"},
			Days:    []time.Weekday{time.Monday},
			Enabled: true,
		}},
		Runs: map[RunKey]RunState{},
	}

	run := func(loc *time.Location) []Decision {
		ex := &recordExec{}
		l, err := New(Options{
			Store:    &fakeStore{snaps: []Snapshot{snap}},
			Executor: ex,
			Serials:  []string{"s"},
			Tick:     time.Millisecond,
			Clock:    func() time.Time { return instant },
			Location: loc,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.tickOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		return ex.calls
	}

	if calls := run(ny); len(calls) != 1 || calls[0].TaskName != "radar" {
		t.Errorf("operator-local tick ran %+v, want one radar — it is Monday evening there", calls)
	}
	// The companion assertion: without it this test would pass against a
	// location that is ignored entirely.
	if calls := run(time.UTC); len(calls) != 0 {
		t.Errorf("UTC tick ran %+v, want nothing — it is already Tuesday in UTC", calls)
	}
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
