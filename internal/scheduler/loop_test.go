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
