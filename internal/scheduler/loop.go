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
