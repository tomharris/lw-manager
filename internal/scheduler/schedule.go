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
