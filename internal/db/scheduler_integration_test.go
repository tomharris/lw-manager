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
	if err := pool.FinishTaskRun(ctx, id1, "succeeded", nil, []int64{}); err != nil {
		t.Fatal(err)
	}
	id2, _ := pool.StartTaskRun(ctx, acctID, "help_all")
	msg := "boom"
	if err := pool.FinishTaskRun(ctx, id2, "failed", &msg, []int64{}); err != nil {
		t.Fatal(err)
	}
	id3, _ := pool.StartTaskRun(ctx, acctID, "help_all")
	if err := pool.FinishTaskRun(ctx, id3, "failed", &msg, []int64{}); err != nil {
		t.Fatal(err)
	}
	// A paused run must neither count as a failure nor reset the streak.
	id4, _ := pool.StartTaskRun(ctx, acctID, "help_all")
	if err := pool.FinishTaskRun(ctx, id4, "paused", nil, []int64{}); err != nil {
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
