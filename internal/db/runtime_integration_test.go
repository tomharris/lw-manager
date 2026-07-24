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
