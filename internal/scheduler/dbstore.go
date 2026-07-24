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
