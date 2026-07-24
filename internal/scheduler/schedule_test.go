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
